package application

import (
	"encoding/json"
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/codegangsta/negroni"
	"github.com/getsentry/raven-go"
	"github.com/gorilla/mux"
	"github.com/jmoiron/jsonq"
	"github.com/meatballhat/negroni-logrus"
	"github.com/rs/cors"
	"github.com/thoas/gokvstores"
	"github.com/thoas/gostorages"
	"github.com/thoas/muxer"
	"github.com/thoas/picfit/engines"
	"github.com/thoas/picfit/extractors"
	"github.com/thoas/picfit/hash"
	"github.com/thoas/picfit/image"
	"github.com/thoas/picfit/middleware"
	"github.com/thoas/stats"
	"io/ioutil"
	"net/http"
	"strings"
)

var Extractors = map[string]extractors.Extractor{
	"op":   extractors.Operation,
	"url":  extractors.URL,
	"path": extractors.Path,
}

type Shard struct {
	Depth int
	Width int
}

type Application struct {
	EnableUpload  bool
	Prefix        string
	SecretKey     string
	KVStore       gokvstores.KVStore
	SourceStorage gostorages.Storage
	DestStorage   gostorages.Storage
	Shard         Shard
	Raven         *raven.Client
	Logger        *logrus.Logger
	Engine        engines.Engine
	Jq            *jsonq.JsonQuery
}

func NewApplication() *Application {
	return &Application{
		Logger:       logrus.New(),
		EnableUpload: false,
	}
}

func NewFromConfigPath(path string) (*Application, error) {
	content, err := ioutil.ReadFile(path)

	if err != nil {
		return nil, fmt.Errorf("Your config file %s cannot be loaded: %s", path, err)
	}

	return NewFromConfig(string(content))
}

func NewFromConfig(content string) (*Application, error) {
	data := map[string]interface{}{}
	dec := json.NewDecoder(strings.NewReader(content))
	err := dec.Decode(&data)

	if err != nil {
		return nil, fmt.Errorf("Your config file %s cannot be parsed: %s", content, err)
	}

	jq := jsonq.NewQuery(data)

	return NewFromJsonQuery(jq)
}

func NewFromJsonQuery(jq *jsonq.JsonQuery) (*Application, error) {
	app := NewApplication()
	app.Jq = jq

	for _, initializer := range Initializers {
		err := initializer(jq, app)

		if err != nil {
			return nil, fmt.Errorf("An error occured during init: %s", err)
		}
	}

	return app, nil
}

func (app *Application) ServeHTTP(h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		con := app.KVStore.Connection()
		defer con.Close()

		res := muxer.NewResponse(w)

		request, err := NewRequest(req, con)

		if err != nil {
			app.Logger.Error(err)

			res.BadRequest()

			return
		}

		if app.SecretKey != "" && !request.IsAuthorized(app.SecretKey) {
			res.Unauthorized()

			return
		}

		h(res, request, app)
	})
}

func (a *Application) InitRouter() *negroni.Negroni {
	router := mux.NewRouter()
	router.NotFoundHandler = NotFoundHandler()

	methods := map[string]Handler{
		"redirect": RedirectHandler,
		"display":  ImageHandler,
		"get":      GetHandler,
	}

	for name, handler := range methods {
		handlerFunc := a.ServeHTTP(handler)

		router.Handle(fmt.Sprintf("/%s", name), handlerFunc)
		router.Handle(fmt.Sprintf("/%s/{sig}/{op}/x{h:[\\d]+}/{path:[\\w\\-/.]+}", name), handlerFunc)
		router.Handle(fmt.Sprintf("/%s/{sig}/{op}/{w:[\\d]+}x/{path:[\\w\\-/.]+}", name), handlerFunc)
		router.Handle(fmt.Sprintf("/%s/{sig}/{op}/{w:[\\d]+}x{h:[\\d]+}/{path:[\\w\\-/.]+}", name), handlerFunc)
		router.Handle(fmt.Sprintf("/%s/{op}/x{h:[\\d]+}/{path:[\\w\\-/.]+}", name), handlerFunc)
		router.Handle(fmt.Sprintf("/%s/{op}/{w:[\\d]+}x/{path:[\\w\\-/.]+}", name), handlerFunc)
		router.Handle(fmt.Sprintf("/%s/{op}/{w:[\\d]+}x{h:[\\d]+}/{path:[\\w\\-/.]+}", name), handlerFunc)
	}

	router.Handle("/upload", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		res := muxer.NewResponse(w)

		UploadHandler(res, req, a)
	}))

	allowedOrigins, err := a.Jq.ArrayOfStrings("allowed_origins")
	allowedMethods, err := a.Jq.ArrayOfStrings("allowed_methods")

	s := stats.New()

	router.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		stats := s.Data()

		b, _ := json.Marshal(stats)

		w.Write(b)
	})

	debug, err := a.Jq.Bool("debug")

	if err != nil {
		debug = false
	}

	n := negroni.New(&middleware.Recovery{
		Raven:      a.Raven,
		Logger:     a.Logger,
		PrintStack: debug,
		StackAll:   false,
		StackSize:  1024 * 8,
	}, &middleware.Logger{a.Logger})
	n.Use(cors.New(cors.Options{
		AllowedOrigins: allowedOrigins,
		AllowedMethods: allowedMethods,
	}))
	n.Use(negronilogrus.NewMiddleware())
	n.UseHandler(router)

	return n
}

func (a *Application) Port() int {
	port, err := a.Jq.Int("port")

	if err != nil {
		port = DefaultPort
	}

	return port
}

func (a *Application) ShardFilename(filename string) string {
	results := hash.Shard(filename, a.Shard.Width, a.Shard.Depth, true)

	return strings.Join(results, "/")
}

func (a *Application) Store(i *image.ImageFile) error {
	con := a.KVStore.Connection()
	defer con.Close()

	err := i.Save()

	if err != nil {
		a.Logger.Fatal(err)
		return err
	}

	a.Logger.Infof("Save thumbnail %s to storage", i.Filepath)

	key := a.WithPrefix(i.Key)

	err = con.Set(key, i.Filepath)

	if err != nil {
		a.Logger.Fatal(err)

		return err
	}

	a.Logger.Infof("Save key %s => %s to kvstore", key, i.Filepath)

	return nil
}

func (a *Application) WithPrefix(str string) string {
	return a.Prefix + str
}

func (a *Application) ImageFileFromRequest(req *Request, async bool, load bool) (*image.ImageFile, error) {
	var file *image.ImageFile = &image.ImageFile{
		Key:     req.Key,
		Storage: a.DestStorage,
		Headers: map[string]string{},
	}
	var err error

	key := a.WithPrefix(req.Key)

	// Image from the KVStore found
	stored, err := gokvstores.String(req.Connection.Get(key))

	file.Filepath = stored

	if stored != "" {
		a.Logger.Infof("Key %s found in kvstore: %s", key, stored)

		if load {
			file, err = image.FromStorage(a.DestStorage, stored)

			if err != nil {
				return nil, err
			}
		}
	} else {
		a.Logger.Infof("Key %s not found in kvstore", key)

		// Image not found from the KVStore, we need to process it
		// URL available in Query String
		if req.URL != nil {
			file, err = image.FromURL(req.URL)
		} else {
			// URL provided we use http protocol to retrieve it
			file, err = image.FromStorage(a.SourceStorage, req.Filepath)
		}

		if err != nil {
			return nil, err
		}

		file, err = a.Engine.Transform(file, req.Operation, req.QueryString)

		if err != nil {
			return nil, err
		}

		file.Filepath = fmt.Sprintf("%s.%s", a.ShardFilename(req.Key), file.Format())
	}

	file.Key = req.Key
	file.Storage = a.DestStorage

	file.Headers["Content-Type"] = file.ContentType()
	file.Headers["ETag"] = req.Key

	if stored == "" {
		if async == true {
			go a.Store(file)
		} else {
			err = a.Store(file)
		}
	}

	return file, err
}
