package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestSource_JSON_round_trip(t *testing.T) {
	tests := []struct {
		name   string
		source domain.Source
		json   string
	}{
		{
			name:   "model client",
			source: domain.ClientSource("inst-alice", "alice"),
			json:   `{"kind":"client","nick":"alice","instance_id":"inst-alice"}`,
		},
		{
			name:   "user client retains its present empty ID",
			source: domain.ClientSource("", "iain"),
			json:   `{"kind":"client","nick":"iain","instance_id":""}`,
		},
		{
			name:   "legacy client has no stable ID",
			source: domain.LegacyClientSource("alice"),
			json:   `{"kind":"client","nick":"alice"}`,
		},
		{
			name:   "server",
			source: domain.ServerSource("modeloff"),
			json:   `{"kind":"server","nick":"modeloff"}`,
		},
		{
			name:   "anonymous",
			source: domain.AnonymousSource(),
			json:   `{"kind":"anonymous"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := tt.source.MarshalJSON()
			require.NoError(t, err)
			require.JSONEq(t, tt.json, string(encoded))

			var got domain.Source
			require.NoError(t, got.UnmarshalJSON(encoded))
			require.Equal(t, tt.source, got)
		})
	}
}

func TestSource_identity(t *testing.T) {
	type sourceProperties struct {
		Nick     domain.Nick
		ID       domain.InstanceID
		IDOK     bool
		IsClient bool
	}

	tests := []struct {
		name       string
		source     domain.Source
		wantNick   domain.Nick
		wantID     domain.InstanceID
		wantIDOK   bool
		wantClient bool
	}{
		{
			name:       "model client",
			source:     domain.ClientSource("inst-alice", "alice"),
			wantNick:   "alice",
			wantID:     "inst-alice",
			wantIDOK:   true,
			wantClient: true,
		},
		{
			name:       "user client",
			source:     domain.ClientSource("", "iain"),
			wantNick:   "iain",
			wantIDOK:   true,
			wantClient: true,
		},
		{
			name:       "legacy client",
			source:     domain.LegacyClientSource("alice"),
			wantNick:   "alice",
			wantClient: true,
		},
		{
			name:     "server",
			source:   domain.ServerSource("modeloff"),
			wantNick: "modeloff",
		},
		{
			name:     "anonymous",
			source:   domain.AnonymousSource(),
			wantNick: domain.AnonymousNick,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotIDOK := tt.source.InstanceID()
			got := sourceProperties{
				Nick:     tt.source.Nick(),
				ID:       gotID,
				IDOK:     gotIDOK,
				IsClient: tt.source.IsClient(),
			}
			want := sourceProperties{tt.wantNick, tt.wantID, tt.wantIDOK, tt.wantClient}

			require.Equal(t, want, got)
		})
	}
}

func TestUnmarshalPersistableEvent_converts_legacy_sources(t *testing.T) {
	const at = "2026-08-24T12:00:00Z"
	wantAt := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		row  string
		want domain.PersistableEvent
	}{
		{
			name: "identified client",
			row:  `{"type":"join","data":{"channel":"#dev","nick":"alice","instance_id":"inst-alice","at":"` + at + `"}}`,
			want: domain.Join{Source: domain.ClientSource("inst-alice", "alice"), Target: "#dev", At: wantAt},
		},
		{
			name: "user client",
			row:  `{"type":"part","data":{"channel":"#dev","nick":"iain","instance_id":"","at":"` + at + `"}}`,
			want: domain.Part{Source: domain.ClientSource("", "iain"), Target: "#dev", At: wantAt},
		},
		{
			name: "client without a stored identity",
			row:  `{"type":"part","data":{"channel":"#dev","nick":"alice","at":"` + at + `"}}`,
			want: domain.Part{Source: domain.LegacyClientSource("alice"), Target: "#dev", At: wantAt},
		},
		{
			name: "anonymous projection",
			row:  `{"type":"message","data":{"channel":"#dev","from":"anonymous","instance_id":"","body":"hello","at":"` + at + `"}}`,
			want: domain.Message{Source: domain.AnonymousSource(), Target: "#dev", Body: "hello", At: wantAt},
		},
		{
			name: "server mode",
			row:  `{"type":"mode_change","data":{"channel":"#dev","flag":110,"add":true,"at":"` + at + `"}}`,
			want: domain.ChannelModeChange{Source: domain.ServerSource(""), Target: "#dev", Flag: domain.ModeNoExternal, Add: true, At: wantAt},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := domain.UnmarshalPersistableEvent([]byte(tt.row))
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
