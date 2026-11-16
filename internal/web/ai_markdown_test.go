package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestAIMarkdownFrontend runs the Node tests of the AI panel's markdown
// renderer, on its source and on the minified module the server serves:
// the renderer is mostly regular expressions, which minification must keep.
func TestAIMarkdownFrontend(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping AI markdown module tests")
	}
	minified, ok := minifiedStatic()["static/js/app/ai-markdown.js"]
	if !ok {
		t.Fatal("ai-markdown.js is not served minified")
	}
	module := filepath.Join(t.TempDir(), "ai-markdown.min.js")
	if err := os.WriteFile(module, minified, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "testdata/ai_markdown_test.mjs")
	cmd.Env = append(os.Environ(), "KULA_AI_MARKDOWN_MINIFIED="+module)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("AI markdown module tests failed: %v\n%s", err, out)
	}
}
