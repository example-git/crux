package proto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceChannelRuntimeReplaceFrame(t *testing.T) {
	proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 2}
	digest, err := config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	proposal.Digest = digest
	frame := WorkspaceChannelFrame{
		Type:      WorkspaceChannelRuntimeReplaceFrame,
		CommandID: "command-1",
		RuntimeReplace: &UpdateRemoteRuntimeRequest{
			ExpectedRevision: 1,
			Runtime:          proposal,
		},
	}
	data, err := json.Marshal(frame)
	require.NoError(t, err)
	decoded, err := DecodeWorkspaceChannelFrame(data)
	require.NoError(t, err)
	require.Equal(t, frame.CommandID, decoded.CommandID)
	require.Equal(t, proposal.Digest, decoded.RuntimeReplace.Runtime.Digest)

	decoded.RuntimeReplace.Runtime.Digest = strings.Repeat("0", 64)
	require.ErrorContains(t, ValidateWorkspaceChannelFrame(decoded), "digest")
	decoded.RuntimeReplace.Runtime = proposal
	decoded.RuntimeReplace.ExpectedRevision = 2
	require.ErrorContains(t, ValidateWorkspaceChannelFrame(decoded), "revision")
}

func TestWorkspaceChannelFrameStrictDecode(t *testing.T) {
	valid := `{"type":"runtime.refresh_complete","command_id":"command-1","runtime_refresh_complete":{"request_id":"refresh-1","failed":true}}`
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "valid", data: []byte(valid)},
		{name: "duplicate", data: []byte(`{"type":"event","type":"event","event":{}}`), want: "duplicate"},
		{name: "unknown field", data: []byte(`{"type":"event","event":{},"unknown":true}`), want: "invalid"},
		{name: "unknown type", data: []byte(`{"type":"other","event":{}}`), want: "unknown"},
		{name: "missing command ID", data: []byte(`{"type":"runtime.refresh_complete","runtime_refresh_complete":{"request_id":"refresh-1"}}`), want: "command ID"},
		{name: "wrong payload", data: []byte(`{"type":"event","runtime_refresh_complete":{"request_id":"refresh-1"}}`), want: "event"},
		{name: "multiple payloads", data: []byte(`{"type":"event","event":{},"runtime_refresh_complete":{"request_id":"refresh-1"}}`), want: "exactly one"},
		{name: "invalid UTF-8", data: []byte{'{', '"', 0xff, '"', '}'}, want: "UTF-8"},
		{name: "oversized", data: make([]byte, MaxWorkspaceChannelFrameBytes+1), want: "byte limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeWorkspaceChannelFrame(test.data)
			if test.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestWorkspaceChannelDecodeRetainsCommandIDForInvalidPayload(t *testing.T) {
	frame, err := DecodeWorkspaceChannelFrame([]byte(`{"type":"runtime.replace","command_id":"command-1","runtime_replace":{"expected_revision":1,"runtime":{"revision":2,"providers":[]}}}`))
	require.Error(t, err)
	require.Equal(t, "command-1", frame.CommandID)
}

func TestWorkspaceChannelAcknowledgementValidation(t *testing.T) {
	tests := []struct {
		name string
		ack  WorkspaceChannelAcknowledgement
		want string
	}{
		{name: "success", ack: WorkspaceChannelAcknowledgement{Status: WorkspaceChannelStatusOK}},
		{name: "structured failure", ack: WorkspaceChannelAcknowledgement{Status: WorkspaceChannelStatusConflict, Message: "runtime revision changed"}},
		{name: "unknown status", ack: WorkspaceChannelAcknowledgement{Status: "retry"}, want: "status"},
		{name: "success message", ack: WorkspaceChannelAcknowledgement{Status: WorkspaceChannelStatusOK, Message: "hidden"}, want: "contains an error"},
		{name: "failure authority", ack: WorkspaceChannelAcknowledgement{Status: WorkspaceChannelStatusForbidden, Authority: &config.RemoteAuthority{Mode: "client"}}, want: "contains authority"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateWorkspaceChannelFrame(WorkspaceChannelFrame{Type: WorkspaceChannelAcknowledgementFrame, CommandID: "command-1", Acknowledgement: &test.ack})
			if test.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.want)
		})
	}
}
