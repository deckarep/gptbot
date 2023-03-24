package milvus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/go-aie/gptbot"
	"github.com/go-aie/xslices"
	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

const (
	idCol, titleCol, headingCol, contentCol, embeddingCol = "id", "title", "heading", "content", "embedding"
)

type Config struct {
	// CollectionName is the collection name.
	// This field is required.
	CollectionName string

	// Addr is the address of the Milvus server.
	// Defaults to "localhost:19530".
	Addr string

	// Dim is the embedding dimension.
	// Defaults to 1536 (the dimension generated by OpenAI's Embedding API).
	Dim int
}

func (cfg *Config) init() {
	if cfg.Addr == "" {
		cfg.Addr = "localhost:19530"
	}
	if cfg.Dim == 0 {
		cfg.Dim = 1536
	}
}

type Milvus struct {
	client client.Client
	cfg    *Config
}

func NewMilvus(cfg *Config) (*Milvus, error) {
	cfg.init()
	ctx := context.Background()

	c, err := client.NewGrpcClient(ctx, cfg.Addr)
	if err != nil {
		return nil, err
	}

	m := &Milvus{
		client: c,
		cfg:    cfg,
	}

	_ = m.client.ReleaseCollection(ctx, m.cfg.CollectionName)
	if err := m.createCollectionIfNotExists(ctx); err != nil {
		return nil, err
	}

	return m, nil
}

func (m *Milvus) LoadJSON(ctx context.Context, filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}

	var sections []gptbot.Section
	if err := json.Unmarshal(data, &sections); err != nil {
		return err
	}

	return m.Insert(ctx, sections)
}

func (m *Milvus) Insert(ctx context.Context, sections []gptbot.Section) error {
	// We need to release the collection before inserting.
	if err := m.client.ReleaseCollection(ctx, m.cfg.CollectionName); err != nil {
		return err
	}

	var ids []int64
	var titles []string
	var headings []string
	var contents []string
	var embeddings [][]float32
	for i, section := range sections {
		ids = append(ids, int64(i))
		titles = append(titles, section.Title)
		headings = append(headings, section.Heading)
		contents = append(contents, section.Content)
		embeddings = append(embeddings, xslices.Float64ToNumber[float32](section.Embedding))
	}

	idColData := entity.NewColumnInt64(idCol, ids)
	titleColData := entity.NewColumnVarChar(titleCol, titles)
	headingColData := entity.NewColumnVarChar(headingCol, headings)
	contentColData := entity.NewColumnVarChar(contentCol, contents)
	embeddingColData := entity.NewColumnFloatVector(embeddingCol, m.cfg.Dim, embeddings)

	// Create index "IVF_FLAT".
	idx, err := entity.NewIndexIvfFlat(entity.L2, 128)
	if err != nil {
		return err
	}
	if err := m.client.CreateIndex(ctx, m.cfg.CollectionName, embeddingCol, idx, false); err != nil {
		return err
	}

	_, err = m.client.Insert(ctx, m.cfg.CollectionName, "", idColData, titleColData, headingColData, contentColData, embeddingColData)
	return err
}

// Query searches similarities of the given embedding with default consistency level.
func (m *Milvus) Query(ctx context.Context, embedding gptbot.Embedding, topK int) ([]*gptbot.Similarity, error) {
	// We need to load the collection before searching.
	if err := m.client.LoadCollection(ctx, m.cfg.CollectionName, false); err != nil {
		return nil, err
	}

	float32Emb := xslices.Float64ToNumber[float32](embedding)
	vec2search := []entity.Vector{
		entity.FloatVector(float32Emb),
	}

	param, _ := entity.NewIndexFlatSearchParam()
	result, err := m.client.Search(
		ctx,
		m.cfg.CollectionName,
		nil,
		"",
		[]string{idCol, titleCol, headingCol, contentCol},
		vec2search,
		embeddingCol,
		entity.L2,
		topK,
		param,
	)
	if err != nil {
		return nil, err
	}

	return constructSimilaritiesFromResult(&result[0])
}

func (m *Milvus) createCollectionIfNotExists(ctx context.Context) error {
	has, err := m.client.HasCollection(ctx, m.cfg.CollectionName)
	if err != nil {
		return err
	}

	if has {
		//_ = m.client.DropCollection(ctx, m.cfg.CollectionName)
		return nil
	}

	// The collection does not exist, so we need to create one.

	schema := &entity.Schema{
		CollectionName: m.cfg.CollectionName,
		AutoID:         false,
		Fields: []*entity.Field{
			{
				Name:       idCol,
				DataType:   entity.FieldTypeInt64,
				PrimaryKey: true,
			},
			{
				Name:     titleCol,
				DataType: entity.FieldTypeVarChar,
				TypeParams: map[string]string{
					entity.TypeParamMaxLength: fmt.Sprintf("%d", 50),
				},
			},
			{
				Name:     headingCol,
				DataType: entity.FieldTypeVarChar,
				TypeParams: map[string]string{
					entity.TypeParamMaxLength: fmt.Sprintf("%d", 50),
				},
			},
			{
				Name:     contentCol,
				DataType: entity.FieldTypeVarChar,
				TypeParams: map[string]string{
					entity.TypeParamMaxLength: fmt.Sprintf("%d", 5000),
				},
			},
			{
				Name:     embeddingCol,
				DataType: entity.FieldTypeFloatVector,
				TypeParams: map[string]string{
					entity.TypeParamDim: fmt.Sprintf("%d", m.cfg.Dim),
				},
			},
		},
	}

	// Create collection with consistency level, which serves as the default search/query consistency level.
	return m.client.CreateCollection(ctx, schema, 2, client.WithConsistencyLevel(entity.ClBounded))
}

func constructSimilaritiesFromResult(result *client.SearchResult) ([]*gptbot.Similarity, error) {
	var iCol *entity.ColumnInt64
	var tCol *entity.ColumnVarChar
	var hCol *entity.ColumnVarChar
	var cCol *entity.ColumnVarChar
	for _, field := range result.Fields {
		switch field.Name() {
		case idCol:
			if c, ok := field.(*entity.ColumnInt64); ok {
				iCol = c
			}
		case titleCol:
			if c, ok := field.(*entity.ColumnVarChar); ok {
				tCol = c
			}
		case headingCol:
			if c, ok := field.(*entity.ColumnVarChar); ok {
				hCol = c
			}
		case contentCol:
			if c, ok := field.(*entity.ColumnVarChar); ok {
				cCol = c
			}
		}
	}

	var similarities []*gptbot.Similarity
	for i := 0; i < result.ResultCount; i++ {
		iVal, err := iCol.ValueByIdx(i)
		if err != nil {
			return nil, err
		}
		tVal, err := tCol.ValueByIdx(i)
		if err != nil {
			return nil, err
		}
		hVal, err := hCol.ValueByIdx(i)
		if err != nil {
			return nil, err
		}
		cVal, err := cCol.ValueByIdx(i)
		if err != nil {
			return nil, err
		}

		similarities = append(similarities, &gptbot.Similarity{
			Section: gptbot.Section{
				Title:   tVal,
				Heading: hVal,
				Content: cVal,
			},
			ID:    int(iVal),
			Score: float64(result.Scores[i]),
		})
	}

	return similarities, nil
}
