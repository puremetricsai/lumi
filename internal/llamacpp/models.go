package llamacpp

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// cacheListTimeout bounds the discovery subprocess. `--cache-list` only reads
// local cache metadata, so anything slower than this is a hung binary.
const cacheListTimeout = 5 * time.Second

// cacheEntry matches a numbered line of `llama-server --cache-list` output:
//
//	number of models in cache: 1
//	   1. unsloth/gemma-4-26B-A4B-it-GGUF:MXFP4_MOE
//
// Matching only numbered entries lets build banners and other log lines through
// the same stream without corrupting the list.
var cacheEntry = regexp.MustCompile(`^\s*\d+\.\s+(\S.*)$`)

// runCacheList is indirected so tests can supply llama-server output without a
// real binary, mirroring the lookPath indirection in server.go.
var runCacheList = func(ctx context.Context, bin string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cacheListTimeout)
	defer cancel()
	// CombinedOutput because older builds log to stderr; parsing ignores noise.
	return exec.CommandContext(ctx, bin, "--cache-list").CombinedOutput()
}

// CachedModels returns the models in llama.cpp's local cache, in the order
// llama-server reports them. Each entry is a `repo:QUANT` id that modelArgs
// passes straight to -hf.
//
// A missing binary, a build too old for --cache-list, or an empty cache all
// yield an empty list: discovery is a convenience for the configure prompt and
// must never fail the caller.
func CachedModels(ctx context.Context) []string {
	bin, ok := Installed()
	if !ok {
		return nil
	}
	out, err := runCacheList(ctx, bin)
	if err != nil {
		// The command may still have printed a partial list before failing, but
		// a non-zero exit means the output is not trustworthy.
		return nil
	}
	return parseCacheList(out)
}

func parseCacheList(out []byte) []string {
	var models []string
	for _, line := range strings.Split(string(out), "\n") {
		if m := cacheEntry.FindStringSubmatch(line); m != nil {
			models = append(models, strings.TrimSpace(m[1]))
		}
	}
	return models
}
