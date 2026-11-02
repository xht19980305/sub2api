package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequestBodyMemoryRejectsOverloadAndReleasesAfterHandler(t *testing.T) {
	router := gin.New()
	held := make(chan struct{})
	release := make(chan struct{})
	router.POST("/v1/responses", RequestBodyLimit(2048, pkghttputil.NewBodyMemoryBudget(1024)), func(c *gin.Context) {
		_, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		if RejectRequestBodyMemoryOverload(c, err) {
			return
		}
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		if c.Query("hold") == "1" {
			close(held)
			<-release
		}
		c.Status(http.StatusNoContent)
	})
	request := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", 700))))
		return w
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- request("/v1/responses?hold=1") }()
	releaseHeld := sync.OnceFunc(func() { close(release) })
	defer releaseHeld()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not reach handler")
	}
	w := request("/v1/responses")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	releaseHeld()
	select {
	case first := <-firstDone:
		require.Equal(t, http.StatusNoContent, first.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("held request did not finish")
	}
	require.Equal(t, http.StatusNoContent, request("/v1/responses").Code)
}
