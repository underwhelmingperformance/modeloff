package clipboard_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
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
