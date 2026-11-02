package httputil

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRequestBodyMemoryBudgetSharedUntilRelease(t *testing.T) {
	budget := NewBodyMemoryBudget(1024)
	ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(bytes.Repeat([]byte{'x'}, 700))).WithContext(ctx)
	got, err := ReadRequestBodyWithPrealloc(req)
	if err != nil || len(got) != 700 {
		t.Fatalf("read: %d %v", len(got), err)
	}
	ctx2, release2 := WithRequestBodyMemoryBudget(context.Background(), budget)
	defer release2()
	req2 := httptest.NewRequest("POST", "/", bytes.NewReader(bytes.Repeat([]byte{'x'}, 700))).WithContext(ctx2)
	if _, err := ReadRequestBodyWithPrealloc(req2); !errors.Is(err, ErrRequestBodyMemoryBudget) {
		t.Fatalf("want capacity error, got %v", err)
	}
	release()
	release() // Idempotent cleanup cannot refund another request's allocation.
	if _, err := ReadRequestBodyWithPrealloc(req2); err != nil {
		t.Fatal(err)
	}
}

func TestRequestBodyMemoryBudgetUnknownLengthAndConcurrency(t *testing.T) {
	budget := NewBodyMemoryBudget(4096)
	ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
	req := httptest.NewRequest("POST", "/", io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{'x'}, 8192)))).WithContext(ctx)
	req.ContentLength = -1
	if _, err := ReadRequestBodyWithPrealloc(req); !errors.Is(err, ErrRequestBodyMemoryBudget) {
		t.Fatalf("unknown length bypassed budget: %v", err)
	}
	release()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
			defer release()
			_ = reserveRequestBodyMemory(ctx, 1024)
		})
	}
	wg.Wait()
	if budget.used != 0 {
		t.Fatalf("leaked reservations: %d", budget.used)
	}
}

func TestDecompressRequestBodyRejectsExcessiveZstdWindow(t *testing.T) {
	for _, exponent := range []byte{20, 24, 29} {
		raw := []byte{0x28, 0xb5, 0x2f, 0xfd, 0, (exponent - 10) << 3, 9, 0, 0, 'x'}
		decoded, err := decompressRequestBody("zstd", raw)
		if exponent == 20 {
			if err != nil || string(decoded) != "x" {
				t.Fatalf("small window: %q %v", decoded, err)
			}
		} else if err == nil {
			t.Fatalf("accepted window 2^%d", exponent)
		}
	}
}

func TestDecompressRequestBodyRejectsOversizeInsteadOfTruncating(t *testing.T) {
	var compressed bytes.Buffer
	encoder := gzip.NewWriter(&compressed)
	chunk := bytes.Repeat([]byte{' '}, 1<<20)
	for range maxDecompressedBodySize / len(chunk) {
		_, _ = encoder.Write(chunk)
	}
	_, _ = encoder.Write([]byte{'x'})
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	decoded, err := decompressRequestBody("gzip", compressed.Bytes())
	var maxErr *http.MaxBytesError
	if decoded != nil || !errors.As(err, &maxErr) || maxErr.Limit != maxDecompressedBodySize {
		t.Fatalf("silently truncated: len=%d err=%v", len(decoded), err)
	}
}

func TestDecodedRequestBodySharesMemoryBudget(t *testing.T) {
	var compressed bytes.Buffer
	encoder := gzip.NewWriter(&compressed)
	_, _ = encoder.Write(bytes.Repeat([]byte{'x'}, 4096))
	_ = encoder.Close()
	ctx, release := WithRequestBodyMemoryBudget(context.Background(), NewBodyMemoryBudget(2048))
	defer release()
	req := httptest.NewRequest("POST", "/", bytes.NewReader(compressed.Bytes())).WithContext(ctx)
	req.Header.Set("Content-Encoding", "gzip")
	if _, err := ReadRequestBodyWithPrealloc(req); !errors.Is(err, ErrRequestBodyMemoryBudget) {
		t.Fatalf("decode bypassed budget: %v", err)
	}
}

func TestNormalizedRequestBodySharesMemoryBudget(t *testing.T) {
	body := []byte(`{"input":"` + strings.Repeat("\x00", 100) + `"}`)
	ctx, release := WithRequestBodyMemoryBudget(context.Background(), NewBodyMemoryBudget(600))
	defer release()
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body)).WithContext(ctx)
	if _, err := ReadLenientJSONRequestBodyWithPrealloc(req, 4096); !errors.Is(err, ErrRequestBodyMemoryBudget) {
		t.Fatalf("normalization bypassed budget: %v", err)
	}
}
