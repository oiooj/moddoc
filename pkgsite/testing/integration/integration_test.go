// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package integration

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v7"
	"golang.org/x/pkgsite/internal"
	"golang.org/x/pkgsite/internal/config"
	"golang.org/x/pkgsite/internal/frontend"
	"golang.org/x/pkgsite/internal/index"
	"golang.org/x/pkgsite/internal/postgres"
	"golang.org/x/pkgsite/internal/proxy"
	"golang.org/x/pkgsite/internal/queue"
	"golang.org/x/pkgsite/internal/source"
	"golang.org/x/pkgsite/internal/testing/testhelper"
	"golang.org/x/pkgsite/internal/worker"
)

var testDB *postgres.DB

func TestMain(m *testing.M) {
	postgres.RunDBTests("discovery_integration_test", m, &testDB)
}

func TestEndToEndProcessing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	defer postgres.ResetTestDB(testDB, t)

	var (
		modulePath = "github.com/my/module"
		version    = "v1.0.0"
		moduleData = map[string]string{
			"go.mod":     "module " + modulePath,
			"foo/foo.go": "package foo\n\nconst Foo = 525600",
			"README.md":  "This is a readme",
			"LICENSE":    testhelper.MITLicense,
		}
	)
	testModules := []*proxy.TestModule{
		{
			ModulePath: modulePath,
			Version:    version,
			Files:      moduleData,
		},
	}
	proxyClient, indexClient, teardownClients := setupProxyAndIndex(t, testModules...)
	defer teardownClients()

	redisCache, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer redisCache.Close()
	redisCacheClient := redis.NewClient(&redis.Options{Addr: redisCache.Addr()})

	redisHA, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer redisHA.Close()
	redisHAClient := redis.NewClient(&redis.Options{Addr: redisHA.Addr()})

	// TODO: it would be better if InMemory made http requests
	// back to worker, rather than calling fetch itself.
	sourceClient := source.NewClient(1 * time.Second)
	queue := queue.NewInMemory(ctx, 10, nil, func(ctx context.Context, mpath, version string) (int, error) {
		return worker.FetchAndUpdateState(ctx, mpath, version, proxyClient, sourceClient, testDB, "test")
	})
	workerServer, err := worker.NewServer(&config.Config{}, worker.ServerConfig{
		DB:                   testDB,
		IndexClient:          indexClient,
		ProxyClient:          proxyClient,
		SourceClient:         source.NewClient(1 * time.Second),
		RedisHAClient:        redisHAClient,
		RedisCacheClient:     redisCacheClient,
		Queue:                queue,
		TaskIDChangeInterval: 10 * time.Minute,
		StaticPath:           "../../../content/static",
	})
	if err != nil {
		t.Fatal(err)
	}
	workerMux := http.NewServeMux()
	workerServer.Install(workerMux.Handle)
	workerHTTP := httptest.NewServer(workerMux)

	frontendServer, err := frontend.NewServer(frontend.ServerConfig{
		DataSource:           testDB,
		Queue:                queue,
		CompletionClient:     redisHAClient,
		TaskIDChangeInterval: 10 * time.Minute,
		StaticPath:           "../../../content/static",
		ThirdPartyPath:       "../../../third_party",
		AppVersionLabel:      "",
	})
	if err != nil {
		t.Fatal(err)
	}
	frontendMux := http.NewServeMux()
	frontendServer.Install(frontendMux.Handle, redisCacheClient)
	frontendHTTP := httptest.NewServer(frontendMux)

	if _, err := doGet(workerHTTP.URL + "/poll"); err != nil {
		t.Fatal(err)
	}
	// TODO: This should really be made deterministic.
	time.Sleep(100 * time.Millisecond)
	if _, err := doGet(workerHTTP.URL + "/enqueue"); err != nil {
		t.Fatal(err)
	}
	// TODO: This should really be made deterministic.
	time.Sleep(100 * time.Millisecond)
	queue.WaitForTesting(ctx)

	body, err := doGet(frontendHTTP.URL + "/github.com/my/module/foo")
	if err != nil {
		t.Fatal(err)
	}
	if idx := strings.Index(string(body), "525600"); idx < 0 {
		t.Error("Documentation constant 525600 not found in body")
	}

	// Populate the auto-completion indexes from the search documents that should
	// have been inserted above.
	if _, err := doGet(workerHTTP.URL + "/update-redis-indexes"); err != nil {
		t.Fatal(err)
	}
	completionBody, err := doGet(frontendHTTP.URL + "/autocomplete?q=foo")
	if err != nil {
		t.Fatal(err)
	}
	completion := "github.com/my/module/foo"
	if idx := strings.Index(string(completionBody), completion); idx < 0 {
		t.Errorf("Auto-completion %q not found in JSON response", completion)
	}
	emptyCompletion, err := doGet(frontendHTTP.URL + "/autocomplete?q=frog")
	if err != nil {
		t.Fatal(err)
	}
	// This could be made more robust by actually parsing the JSON.
	if got := string(emptyCompletion); got != "[]" {
		t.Errorf("GET /autocomplete?q=frog: expected empty results, got %q", got)
	}
}

// doGet executes an HTTP GET request for url and returns the response body, or
// an error if anything went wrong or the response status code was not 200 OK.
func doGet(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("http.Get(%q): %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http.Get(%q): status: %d, want %d", url, resp.StatusCode, http.StatusOK)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ioutil.ReadAll(): %v", err)
	}
	return body, nil
}

func setupProxyAndIndex(t *testing.T, modules ...*proxy.TestModule) (*proxy.Client, *index.Client, func()) {
	t.Helper()
	proxyClient, teardownProxy := proxy.SetupTestProxy(t, modules)
	var indexVersions []*internal.IndexVersion
	for _, m := range modules {
		indexVersions = append(indexVersions, &internal.IndexVersion{
			Path:      m.ModulePath,
			Version:   m.Version,
			Timestamp: time.Now(),
		})
	}
	indexClient, teardownIndex := index.SetupTestIndex(t, indexVersions)
	teardown := func() {
		teardownProxy()
		teardownIndex()
	}
	return proxyClient, indexClient, teardown
}
