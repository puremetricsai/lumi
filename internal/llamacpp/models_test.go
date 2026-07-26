package llamacpp

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestParseCacheList(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []string
	}{
		{
			name: "llama-server output",
			out: "number of models in cache: 2\n" +
				"   1. unsloth/gemma-4-26B-A4B-it-GGUF:MXFP4_MOE\n" +
				"   2. ggml-org/gpt-oss-20b-GGUF:Q4_K_M\n",
			want: []string{
				"unsloth/gemma-4-26B-A4B-it-GGUF:MXFP4_MOE",
				"ggml-org/gpt-oss-20b-GGUF:Q4_K_M",
			},
		},
		{
			name: "empty cache",
			out:  "number of models in cache: 0\n",
			want: nil,
		},
		{
			// CombinedOutput may carry build/log lines; only numbered entries count.
			name: "interleaved log noise",
			out: "build: 6100 (abc1234) with clang\n" +
				"number of models in cache: 1\n" +
				"   1. unsloth/gemma-4-26B-A4B-it-GGUF:MXFP4_MOE\n" +
				"main: cleaning up\n",
			want: []string{"unsloth/gemma-4-26B-A4B-it-GGUF:MXFP4_MOE"},
		},
		{
			// An old build without --cache-list prints usage, not a list.
			name: "unrecognized flag",
			out:  "error: invalid argument: --cache-list\nusage: llama-server [options]\n",
			want: nil,
		},
		{name: "empty output", out: "", want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCacheList([]byte(c.out)); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseCacheList() = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestCachedModelsNotInstalled(t *testing.T) {
	orig := lookPath
	defer func() { lookPath = orig }()
	lookPath = func(string) (string, error) { return "", errors.New("not found") }

	if got := CachedModels(context.Background()); len(got) != 0 {
		t.Fatalf("CachedModels() with no binary = %#v, want empty", got)
	}
}

func TestCachedModelsCommandFailure(t *testing.T) {
	origLook, origRun := lookPath, runCacheList
	defer func() { lookPath, runCacheList = origLook, origRun }()
	lookPath = func(string) (string, error) { return "/usr/local/bin/llama-server", nil }
	// A failing command still yields a usable (empty) list: discovery is a
	// convenience and must never fail the caller.
	runCacheList = func(context.Context, string) ([]byte, error) {
		return []byte("error: unknown flag\n"), errors.New("exit status 1")
	}

	if got := CachedModels(context.Background()); len(got) != 0 {
		t.Fatalf("CachedModels() on command failure = %#v, want empty", got)
	}
}

func TestCachedModelsParsesRunnerOutput(t *testing.T) {
	origLook, origRun := lookPath, runCacheList
	defer func() { lookPath, runCacheList = origLook, origRun }()
	lookPath = func(string) (string, error) { return "/usr/local/bin/llama-server", nil }
	runCacheList = func(_ context.Context, bin string) ([]byte, error) {
		if bin != "/usr/local/bin/llama-server" {
			t.Fatalf("ran %q, want the resolved binary", bin)
		}
		return []byte("number of models in cache: 1\n   1. repo/model-GGUF:Q8_0\n"), nil
	}

	want := []string{"repo/model-GGUF:Q8_0"}
	if got := CachedModels(context.Background()); !reflect.DeepEqual(got, want) {
		t.Fatalf("CachedModels() = %#v, want %#v", got, want)
	}
}
