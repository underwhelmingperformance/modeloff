package clipboard_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui/clipboard"
)

func TestCopyCmdEmitsOSC52Sequence(t *testing.T) {
	var buf bytes.Buffer
	restore := clipboard.SetWriter(&buf)
	t.Cleanup(restore)

	cmd := clipboard.CopyCmd("hello, world")
	require.NotNil(t, cmd)
	require.Nil(t, cmd(), "the cmd has no follow-up message")

	want := fmt.Sprintf("\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte("hello, world")))
	require.Equal(t, want, buf.String())
}

func TestCopyCmdEmptyTextReturnsNilCmd(t *testing.T) {
	require.Nil(t, clipboard.CopyCmd(""))
}

// TestCopyCmd_concurrent_with_SetWriter exercises the exact race
// Bubble Tea can produce in production: it runs each Cmd it returns
// on its own goroutine, so a CopyCmd invocation and a concurrent
// SetWriter (e.g. from a test, or a future reconfiguration) can touch
// the package-global writer at the same time. Run with -race.
func TestCopyCmd_concurrent_with_SetWriter(t *testing.T) {
	restore := clipboard.SetWriter(io.Discard)
	t.Cleanup(restore)

	var wg sync.WaitGroup

	for range 20 {
		wg.Add(2)

		go func() {
			defer wg.Done()
			clipboard.CopyCmd("concurrent")()
		}()

		go func() {
			defer wg.Done()
			r := clipboard.SetWriter(io.Discard)
			r()
		}()
	}

	wg.Wait()
}
