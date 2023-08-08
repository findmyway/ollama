package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"gonum.org/v1/gonum/mat"

	"github.com/jmorganca/ollama/api"
	"github.com/jmorganca/ollama/llama"
	"github.com/jmorganca/ollama/vector"
)

var loaded struct {
	mu sync.Mutex

	llm        *llama.LLM
	Embeddings []vector.Embedding

	expireAt    time.Time
	expireTimer *time.Timer

	digest  string
	options api.Options
}

// load a model into memory if it is not already loaded, it is up to the caller to lock loaded.mu before calling this function
func load(model *Model, reqOpts map[string]interface{}, sessionDuration time.Duration) error {
	opts := api.DefaultOptions()
	if err := opts.FromMap(model.Options); err != nil {
		log.Printf("could not load model options: %v", err)
		return err
	}

	if err := opts.FromMap(reqOpts); err != nil {
		log.Printf("could not merge model options: %v", err)
		return err
	}

	if model.Digest != loaded.digest || !reflect.DeepEqual(loaded.options, opts) {
		if loaded.llm != nil {
			loaded.llm.Close()
			loaded.llm = nil
			loaded.digest = ""
		}

		if model.Embeddings != nil && len(model.Embeddings) > 0 {
			opts.EmbeddingOnly = true // this is requried to generate embeddings, completions will still work
			loaded.Embeddings = model.Embeddings
		}

		llm, err := llama.New(model.ModelPath, opts)
		if err != nil {
			return err
		}

		loaded.llm = llm
		loaded.digest = model.Digest
		loaded.options = opts
	}
	loaded.expireAt = time.Now().Add(sessionDuration)

	if loaded.expireTimer == nil {
		loaded.expireTimer = time.AfterFunc(sessionDuration, func() {
			loaded.mu.Lock()
			defer loaded.mu.Unlock()

			if time.Now().Before(loaded.expireAt) {
				return
			}

			if loaded.llm == nil {
				return
			}

			loaded.llm.Close()
			loaded.llm = nil
			loaded.digest = ""
		})
	}
	loaded.expireTimer.Reset(sessionDuration)
	return nil
}

func GenerateHandler(c *gin.Context) {
	loaded.mu.Lock()
	defer loaded.mu.Unlock()

	checkpointStart := time.Now()

	var req api.GenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	model, err := GetModel(req.Model)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sessionDuration := 5 * time.Minute
	if err := load(model, req.Options, sessionDuration); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	checkpointLoaded := time.Now()

	embedding := ""
	if model.Embeddings != nil && len(model.Embeddings) > 0 {
		promptEmbed, err := loaded.llm.Embedding(req.Prompt)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// TODO: set embed_top from specified parameters in modelfile
		embed_top := 3
		topK := vector.TopK(embed_top, mat.NewVecDense(len(promptEmbed), promptEmbed), loaded.Embeddings)
		for _, e := range topK {
			embedding = fmt.Sprintf("%s %s", embedding, e.Embedding.Data)
		}
	}

	prompt, err := model.Prompt(req, embedding)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.GenerateResponse) {
			loaded.expireAt = time.Now().Add(sessionDuration)
			loaded.expireTimer.Reset(sessionDuration)

			r.Model = req.Model
			r.CreatedAt = time.Now().UTC()
			if r.Done {
				r.TotalDuration = time.Since(checkpointStart)
				r.LoadDuration = checkpointLoaded.Sub(checkpointStart)
			}

			ch <- r
		}

		if err := loaded.llm.Predict(req.Context, prompt, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	streamResponse(c, ch)
}

func EmbeddingHandler(c *gin.Context) {
	loaded.mu.Lock()
	defer loaded.mu.Unlock()

	var req api.EmbeddingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	model, err := GetModel(req.Model)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := load(model, req.Options, 5*time.Minute); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !loaded.options.EmbeddingOnly {
		c.JSON(http.StatusBadRequest, gin.H{"error": "embedding option must be set to true"})
		return
	}

	retry := 0
generate:
	if retry > 3 {
		log.Printf("failed to generate embedding for '%s': %v", req.Model, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate embedding"})
		return
	}
	embedding, err := loaded.llm.Embedding(req.Prompt)
	if err != nil {
		log.Printf("retrying embedding generation for '%s': %v", req.Model, err)
		retry++
		goto generate
	}
	// Check for NaN and Inf in the embedding, which can't be stored
	for _, value := range embedding {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			log.Printf("reloading model, embedding contains NaN or Inf")
			// reload the model to get a new embedding, the seed can effect these outputs and reloading changes it
			loaded.llm.Close()
			loaded.llm = nil
			loaded.digest = ""
			if err := load(model, req.Options, 5*time.Minute); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			retry++
			goto generate
		}
	}

	resp := api.EmbeddingResponse{
		Embedding: embedding,
	}
	c.JSON(http.StatusOK, resp)
}

func PullModelHandler(c *gin.Context) {
	var req api.PullRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.ProgressResponse) {
			ch <- r
		}

		regOpts := &RegistryOptions{
			Insecure: req.Insecure,
			Username: req.Username,
			Password: req.Password,
		}

		if err := PullModel(req.Name, regOpts, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	streamResponse(c, ch)
}

func PushModelHandler(c *gin.Context) {
	var req api.PushRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(r api.ProgressResponse) {
			ch <- r
		}

		regOpts := &RegistryOptions{
			Insecure: req.Insecure,
			Username: req.Username,
			Password: req.Password,
		}

		if err := PushModel(req.Name, regOpts, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	streamResponse(c, ch)
}

func CreateModelHandler(c *gin.Context) {
	var req api.CreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"message": err.Error()})
		return
	}

	ch := make(chan any)
	go func() {
		defer close(ch)
		fn := func(resp api.ProgressResponse) {
			ch <- resp
		}

		if err := CreateModel(req.Name, req.Path, fn); err != nil {
			ch <- gin.H{"error": err.Error()}
		}
	}()

	streamResponse(c, ch)
}

func DeleteModelHandler(c *gin.Context) {
	var req api.DeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := DeleteModel(req.Name); err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Name)})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
}

func ListModelsHandler(c *gin.Context) {
	var models []api.ListResponseModel
	fp, err := GetManifestPath()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	err = filepath.Walk(fp, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				log.Printf("manifest file does not exist: %s", fp)
				return nil
			}
			return err
		}
		if !info.IsDir() {
			fi, err := os.Stat(path)
			if err != nil {
				log.Printf("skipping file: %s", fp)
				return nil
			}
			path := path[len(fp)+1:]
			slashIndex := strings.LastIndex(path, "/")
			if slashIndex == -1 {
				return nil
			}
			tag := path[:slashIndex] + ":" + path[slashIndex+1:]
			mp := ParseModelPath(tag)
			manifest, err := GetManifest(mp)
			if err != nil {
				log.Printf("skipping file: %s", fp)
				return nil
			}
			model := api.ListResponseModel{
				Name:       mp.GetShortTagname(),
				Size:       manifest.GetTotalSize(),
				ModifiedAt: fi.ModTime(),
			}
			models = append(models, model)
		}
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, api.ListResponse{Models: models})
}

func CopyModelHandler(c *gin.Context) {
	var req api.CopyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := CopyModel(req.Source, req.Destination); err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Source)})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
}

func Serve(ln net.Listener, extraOrigins []string) error {
	config := cors.DefaultConfig()
	config.AllowWildcard = true
	// only allow http/https from localhost
	allowedOrigins := []string{
		"http://localhost",
		"http://localhost:*",
		"https://localhost",
		"https://localhost:*",
		"http://127.0.0.1",
		"http://127.0.0.1:*",
		"https://127.0.0.1",
		"https://127.0.0.1:*",
	}
	allowedOrigins = append(allowedOrigins, extraOrigins...)
	config.AllowOrigins = allowedOrigins

	r := gin.Default()
	r.Use(cors.New(config))

	r.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, "Ollama is running")
	})
	r.HEAD("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	r.POST("/api/pull", PullModelHandler)
	r.POST("/api/generate", GenerateHandler)
	r.POST("/api/embedding", EmbeddingHandler)
	r.POST("/api/create", CreateModelHandler)
	r.POST("/api/push", PushModelHandler)
	r.POST("/api/copy", CopyModelHandler)
	r.GET("/api/tags", ListModelsHandler)
	r.DELETE("/api/delete", DeleteModelHandler)

	log.Printf("Listening on %s", ln.Addr())
	s := &http.Server{
		Handler: r,
	}

	return s.Serve(ln)
}

func streamResponse(c *gin.Context, ch chan any) {
	c.Stream(func(w io.Writer) bool {
		val, ok := <-ch
		if !ok {
			return false
		}

		bts, err := json.Marshal(val)
		if err != nil {
			log.Printf("streamResponse: json.Marshal failed with %s", err)
			return false
		}

		bts = append(bts, '\n')
		if _, err := w.Write(bts); err != nil {
			log.Printf("streamResponse: w.Write failed with %s", err)
			return false
		}

		return true
	})
}
