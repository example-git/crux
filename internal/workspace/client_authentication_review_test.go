package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func authenticationReviewAction(operation string, target providerauth.Target, sequence uint64) clientAuthenticationReviewRequest {
	return clientAuthenticationReviewRequest{OperationID: operation, OriginalTarget: target, ReviewID: fmt.Sprintf("%032x", sequence+20000), ReviewSequence: sequence}
}

func authenticationReviewApply(request clientAuthenticationReviewRequest, summary clientAuthenticationReviewSummary) clientAuthenticationApplyRequest {
	return clientAuthenticationApplyRequest{OperationID: request.OperationID, OriginalTarget: request.OriginalTarget, ReviewID: request.ReviewID, PreviewID: summary.PreviewID, ApplyID: fmt.Sprintf("%032x", request.ReviewSequence+30000)}
}

func rejectedAuthenticationReviewFixture(t *testing.T, barrier bool) (*clientAuthenticationFixture, providerauth.SwitchRequest) {
	t.Helper()
	f := newClientAuthenticationFixture(t, barrier)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("b", 32), Target: f.target(t), AccountID: f.second.ID}
	f.putMode.Store(1)
	_, err := f.w.switchClientAuthentication(t.Context(), request)
	require.Error(t, err)
	f.putMode.Store(0)
	return f, request
}

func TestClientAuthenticationReviewApplyPartialLogoutTLS(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	require.NoError(t, f.w.InitCoderAgentNonInteractive(t.Context()))
	receiver, err := f.s.Backend().GetWorkspace(f.w.workspaceID())
	require.NoError(t, err)
	coordinator := receiver.App.CurrentAgentCoordinator()
	old := coordinator.Model()
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("reviewed logout")}}
	_, err = old.Model.Generate(t.Context(), call)
	require.NoError(t, err)
	request := providerauth.LogoutRequest{OperationID: strings.Repeat("a", 32), Target: f.target(t)}
	f.store.SetRuntimeGenerationPreparer(func(context.Context, config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() {
			data, err := os.ReadFile(f.path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
		}}, nil
	})
	originalOutcome, err := f.w.logoutClientAuthentication(t.Context(), request)
	require.ErrorIs(t, err, providerauth.ErrMutation)
	require.True(t, originalOutcome.Progress.RuntimePublished)
	require.Nil(t, originalOutcome.Change)
	f.store.SetRuntimeGenerationPreparer(nil)
	model := f.store.RuntimeSnapshot().AgentModelState().Large.Model
	model.MaxTokens++
	_, err = f.store.OverrideModelsForOwners(config.AgentModelState{Large: &config.OwnedSelectedModel{Model: model, Owner: f.owner}})
	require.NoError(t, err, "an explicit local model edit is accepted separately")
	models := f.store.RuntimeSnapshot().AgentModelState()
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	view := f.w.Config()
	reviewRequest := authenticationReviewAction(request.OperationID, request.Target, 1)
	summary, err := f.w.reviewClientAuthentication(t.Context(), reviewRequest)
	require.NoError(t, err)
	require.NotEmpty(t, summary.PreviewID)
	require.Contains(t, summary.ChangedCategories, "selected models")
	require.Zero(t, f.puts.Load(), "review does not publish")
	require.Same(t, view, f.w.Config())
	requests := f.requests.Load()
	f.getMode.Store(1)
	again, err := f.w.reviewClientAuthentication(t.Context(), reviewRequest)
	require.NoError(t, err)
	require.Equal(t, summary, again)
	require.Equal(t, requests, f.requests.Load(), "a repeated review does not GET or collect")
	f.getMode.Store(0)
	apply := authenticationReviewApply(reviewRequest, summary)
	outcome, err := f.w.applyClientAuthenticationReview(t.Context(), apply)
	require.NoError(t, err)
	require.True(t, outcome.RemoteAcknowledged && outcome.Adopted)
	require.Equal(t, "runtime-reconciled", outcome.OriginalDisposition)
	require.EqualValues(t, 1, f.puts.Load())
	require.Equal(t, models, receiver.Cfg.RuntimeSnapshot().AgentModelState())
	for _, current := range []agent.Model{old, coordinator.Model()} {
		_, err = current.Model.Generate(t.Context(), call)
		require.Error(t, err)
	}
	require.Equal(t, []string{"Bearer " + f.first.AccessToken}, f.observed())
	original := f.w.authority.authenticationReceipts[request.OperationID]
	require.Equal(t, originalOutcome, original.outcome)
	require.False(t, original.acknowledged || original.adopted, "separate publication does not falsify the original receipt")
	require.Equal(t, summary.PreviewID, original.reconciledBy)
	require.True(t, f.w.authority.removed[f.owner])
	require.False(t, f.w.authority.unacknowledgedClientAuthentication(f.w.workspaceID()))
	againOutcome, err := f.w.applyClientAuthenticationReview(t.Context(), apply)
	require.NoError(t, err)
	require.Equal(t, outcome, againOutcome)
	oldOutcome, err := f.w.logoutClientAuthentication(t.Context(), request)
	require.ErrorContains(t, err, "separate reviewed action")
	require.Equal(t, originalOutcome, oldOutcome)
	_, err = f.w.recoverClientAuthentication(t.Context(), clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1))
	require.ErrorContains(t, err, "separate reviewed action")
	require.EqualValues(t, 1, f.puts.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
	require.NoError(t, f.w.SetCompactMode(config.ScopeGlobal, true), "generic presentation resumes after distinct adoption")
}

func TestClientAuthenticationReviewApplySwitchChoiceTLS(t *testing.T) {
	for _, alternate := range []bool{false, true} {
		t.Run(fmt.Sprint(alternate), func(t *testing.T) {
			f, request := rejectedAuthenticationReviewFixture(t, false)
			original := f.w.authority.authenticationReceipts[request.OperationID]
			originalOutcome := original.outcome
			require.NoError(t, f.w.InitCoderAgentNonInteractive(t.Context()))
			want := f.second
			if alternate {
				capture, err := f.store.CaptureAuthentication(t.Context())
				require.NoError(t, err)
				_, err = f.store.SwitchAuthenticationAccount(t.Context(), config.ScopeGlobal, capture, f.owner, f.first.ID)
				require.NoError(t, err, "separately explicit local choice saved account A")
				want = f.first
			}
			review := authenticationReviewAction(request.OperationID, request.Target, 1)
			summary, err := f.w.reviewClientAuthentication(t.Context(), review)
			if alternate {
				require.ErrorIs(t, err, config.ErrAuthenticationReconciliationConflict)
				require.Empty(t, summary.PreviewID)
				review = authenticationReviewAction(request.OperationID, request.Target, 2)
				review.Choice = clientAuthenticationReviewChoice{Kind: "saved-account", AccountID: f.first.ID}
				summary, err = f.w.reviewClientAuthentication(t.Context(), review)
			}
			require.NoError(t, err)
			require.Equal(t, want.ID, summary.ActiveAccountID)
			paths := []string{f.path, f.accountsPath}
			infos, bodies := clientAuthenticationFiles(t, paths...)
			outcome, err := f.w.applyClientAuthenticationReview(t.Context(), authenticationReviewApply(review, summary))
			require.NoError(t, err)
			require.True(t, outcome.Adopted)
			receiver, err := f.s.Backend().GetWorkspace(f.w.workspaceID())
			require.NoError(t, err)
			_, err = receiver.App.CurrentAgentCoordinator().Model().Model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("reviewed switch")}})
			require.NoError(t, err)
			require.Equal(t, []string{"Bearer " + want.AccessToken}, f.observed())
			require.NoFileExists(t, f.marker)
			require.Equal(t, originalOutcome, original.outcome)
			require.False(t, original.acknowledged || original.adopted)
			requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
		})
	}
}

func TestClientAuthenticationReviewApplyFences(t *testing.T) {
	for _, change := range []string{"file", "account", "publication", "cache-workspace", "cache-principal", "receiver", "newer-review", "wrong-preview"} {
		t.Run(change, func(t *testing.T) {
			f, request := rejectedAuthenticationReviewFixture(t, false)
			review := authenticationReviewAction(request.OperationID, request.Target, 1)
			summary, err := f.w.reviewClientAuthentication(t.Context(), review)
			require.NoError(t, err)
			apply := authenticationReviewApply(review, summary)
			switch change {
			case "file":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
			case "account":
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.first))
			case "publication":
				_, err = f.store.OverrideModelsForOwners(f.store.RuntimeSnapshot().AgentModelState())
				require.NoError(t, err)
			case "cache-workspace", "cache-principal":
				f.w.mu.Lock()
				if change == "cache-workspace" {
					f.w.ws.ID = "different-workspace"
				} else {
					copy := *f.w.ws.Authority
					copy.Principal = "different-principal"
					f.w.ws.Authority = &copy
				}
				f.w.mu.Unlock()
			case "receiver":
				receipt := f.w.authority.authenticationReviews[review.ReviewID]
				_, err = f.w.client.ReplaceRemoteRuntime(t.Context(), request.Target.WorkspaceID, receipt.base.Revision, *receipt.proposal)
				require.NoError(t, err)
			case "newer-review":
				_, err = f.w.reviewClientAuthentication(t.Context(), authenticationReviewAction(request.OperationID, request.Target, 2))
				require.NoError(t, err)
			case "wrong-preview":
				apply.PreviewID = strings.Repeat("f", 32)
			}
			puts := f.puts.Load()
			outcome, err := f.w.applyClientAuthenticationReview(t.Context(), apply)
			require.Error(t, err)
			require.False(t, outcome.RemoteAcknowledged || outcome.Adopted)
			require.Equal(t, puts, f.puts.Load())
		})
	}
}

func TestClientAuthenticationReviewRequiresExplicitReload(t *testing.T) {
	f, request := rejectedAuthenticationReviewFixture(t, false)
	marker := filepath.Join(f.root, "shell-review-marker")
	require.NoError(t, os.WriteFile(filepath.Join(f.root, ".cruxrc"), []byte(fmt.Sprintf("printf x >> '%s'; printf '{}'", marker)), 0o600))
	review := authenticationReviewAction(request.OperationID, request.Target, 1)
	_, err := f.w.reviewClientAuthentication(t.Context(), review)
	require.ErrorIs(t, err, config.ErrAuthenticationReconciliationReloadRequired)
	require.NoFileExists(t, marker)
	requests := f.requests.Load()
	_, err = f.w.reviewClientAuthentication(t.Context(), review)
	require.ErrorIs(t, err, config.ErrAuthenticationReconciliationReloadRequired)
	require.Equal(t, requests, f.requests.Load())
	require.NoError(t, f.store.ReloadFromDisk(t.Context()), "normal reload is a separate explicit action")
	count, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.NotEmpty(t, count)
	review = authenticationReviewAction(request.OperationID, request.Target, 2)
	summary, err := f.w.reviewClientAuthentication(t.Context(), review)
	require.NoError(t, err)
	_, err = f.w.applyClientAuthenticationReview(t.Context(), authenticationReviewApply(review, summary))
	require.NoError(t, err)
	after, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, count, after, "review/apply never rerun shell config")
}

func TestClientAuthenticationReviewLostAckAndExplicitReplay(t *testing.T) {
	f, request := rejectedAuthenticationReviewFixture(t, false)
	review := authenticationReviewAction(request.OperationID, request.Target, 1)
	summary, err := f.w.reviewClientAuthentication(t.Context(), review)
	require.NoError(t, err)
	apply := authenticationReviewApply(review, summary)
	f.putMode.Store(2)
	change := func() { f.getMode.Store(1) }
	f.afterPut.Store(&change)
	outcome, err := f.w.applyClientAuthenticationReview(t.Context(), apply)
	require.Error(t, err)
	require.False(t, outcome.Adopted)
	require.EqualValues(t, 2, f.puts.Load())
	a := f.w.authority
	original := a.authenticationReceipts[request.OperationID]
	require.Empty(t, original.reconciledBy)
	requests := f.requests.Load()
	a.mu.Lock()
	err = f.w.reconcileClientAuthority(t.Context(), a)
	a.mu.Unlock()
	require.ErrorContains(t, err, "reviewed authentication")
	_, err = f.w.recoverClientAuthentication(t.Context(), clientAuthenticationRecoveryAction(request.OperationID, request.Target, 1))
	require.ErrorContains(t, err, "reviewed authentication")
	_, err = f.w.switchClientAuthentication(t.Context(), request)
	require.ErrorContains(t, err, "reviewed authentication")
	require.Equal(t, requests, f.requests.Load(), "generic and phase-one replay cannot adopt the separate reviewed proposal")
	f.getMode.Store(0)
	f.afterPut.Store(nil)
	remote, err := f.w.client.GetWorkspace(t.Context(), request.Target.WorkspaceID)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a.mu.Lock()
	outcome, err = f.w.completeAuthenticationReviewApply(ctx, a, original, a.authenticationReviews[review.ReviewID], remote.Authority)
	a.mu.Unlock()
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, outcome.RemoteAcknowledged)
	require.False(t, outcome.Adopted)
	require.Empty(t, original.reconciledBy)
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	outcome, err = f.w.applyClientAuthenticationReview(t.Context(), apply)
	require.NoError(t, err)
	require.True(t, outcome.RemoteAcknowledged && outcome.Adopted)
	require.EqualValues(t, 2, f.puts.Load(), "replay must only GET")
	require.False(t, original.acknowledged || original.adopted)
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
}

func TestClientAuthenticationReviewSequenceAndEviction(t *testing.T) {
	f, request := rejectedAuthenticationReviewFixture(t, false)
	review := authenticationReviewAction(request.OperationID, request.Target, 1)
	summary, err := f.w.reviewClientAuthentication(t.Context(), review)
	require.NoError(t, err)
	f.putMode.Store(1)
	apply := authenticationReviewApply(review, summary)
	_, err = f.w.applyClientAuthenticationReview(t.Context(), apply)
	require.Error(t, err)
	other := apply
	other.ApplyID = strings.Repeat("e", 32)
	_, err = f.w.applyClientAuthenticationReview(t.Context(), other)
	require.ErrorIs(t, err, providerauth.ErrOperationConflict)
	f.getMode.Store(1)
	for sequence := uint64(2); sequence <= clientAuthenticationReceiptLimit+2; sequence++ {
		_, err = f.w.reviewClientAuthentication(t.Context(), authenticationReviewAction(request.OperationID, request.Target, sequence))
		require.Error(t, err)
	}
	require.Len(t, f.w.authority.authenticationReviews, clientAuthenticationReceiptLimit)
	require.Nil(t, f.w.authority.authenticationReviews[review.ReviewID])
	requests := f.requests.Load()
	_, err = f.w.reviewClientAuthentication(t.Context(), review)
	require.ErrorIs(t, err, providerauth.ErrStale)
	_, err = f.w.applyClientAuthenticationReview(t.Context(), apply)
	require.ErrorIs(t, err, providerauth.ErrStale)
	f.w.authority.mu.Lock()
	err = f.w.reconcileClientAuthority(t.Context(), f.w.authority)
	f.w.authority.mu.Unlock()
	require.ErrorContains(t, err, "reviewed authentication", "eviction cannot erase pending-publication guard")
	require.Equal(t, requests, f.requests.Load())
	f.getMode.Store(0)
	f.putMode.Store(0)
	review = authenticationReviewAction(request.OperationID, request.Target, clientAuthenticationReceiptLimit+3)
	summary, err = f.w.reviewClientAuthentication(t.Context(), review)
	require.NoError(t, err, "bounded retention is not a capacity veto")
	_, err = f.w.applyClientAuthenticationReview(t.Context(), authenticationReviewApply(review, summary))
	require.NoError(t, err)
}

func TestClientAuthenticationReviewPrivacyAndCancelableLocks(t *testing.T) {
	f, request := rejectedAuthenticationReviewFixture(t, false)
	review := authenticationReviewAction(request.OperationID, request.Target, 1)
	summary, err := f.w.reviewClientAuthentication(t.Context(), review)
	require.NoError(t, err)
	encoded, err := json.Marshal(summary)
	require.NoError(t, err)
	for _, secret := range []string{f.first.AccessToken, f.second.AccessToken, f.marker, "account_namespace"} {
		require.NotContains(t, string(encoded), secret)
	}
	private := f.w.authority.authenticationReviews[review.ReviewID]
	_, err = json.Marshal(private)
	require.Error(t, err)
	require.NotContains(t, fmt.Sprintf("%#v", private), f.second.AccessToken)
	summary.Models[0].Model = "caller-mutated"
	summary.Receiver.Accounts = nil
	again, err := f.w.reviewClientAuthentication(t.Context(), review)
	require.NoError(t, err)
	require.NotEqual(t, summary.Models, again.Models)
	for _, workspaceLock := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		var unlock func()
		if workspaceLock {
			f.w.mu.Lock()
			unlock = f.w.mu.Unlock
		} else {
			f.w.authority.mu.Lock()
			unlock = f.w.authority.mu.Unlock
		}
		done := make(chan error, 1)
		go func() { _, err := f.w.reviewClientAuthentication(ctx, review); done <- err }()
		cancel()
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			unlock()
			t.Fatal("review did not cancel while its mutex remained held")
		}
		unlock()
	}
	f.w.subCancel()
	_, err = f.w.reviewClientAuthentication(t.Context(), review)
	require.ErrorIs(t, err, context.Canceled)
}

func TestClientAuthenticationReviewChangeSummary(t *testing.T) {
	for _, tc := range []struct{ label, before, after string }{
		{"runtime format", `{}`, `{"version":2}`},
		{"selected models", `{}`, `{"models":{"large":{"model":"selected"}}}`},
		{"execution controls", `{}`, `{"controls":{"response_verbosity":"high"}}`},
		{"provider settings", `{}`, `{"providers":[{"config":{"id":"private-provider"}}]}`},
		{"image settings", `{}`, `{"images":{}}`},
		{"authentication", `{"credentials":[{"account":{"raw":{"number":1}}}]}`, `{"credentials":[{"account":{"raw":{"number":1.0}}}]}`},
		{"provider instructions", `{}`, `{"provider_context_instructions":{"provider":"private instructions"}}`},
		{"credential environment", `{}`, `{"credential_environment":{"KEY":"private value"}}`},
		{"provider bundles", `{}`, `{"bundles":[{"digest":"different","files":[{"path":"manifest.json","data":"cHJpdmF0ZQ=="}]}]}`},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var before, after config.RemoteRuntimeProposal
			require.NoError(t, json.Unmarshal([]byte(tc.before), &before))
			require.NoError(t, json.Unmarshal([]byte(tc.after), &after))
			changed, err := authenticationReviewChangedCategories(before, after)
			require.NoError(t, err)
			require.Equal(t, []string{tc.label}, changed)
			changed, err = authenticationReviewChangedCategories(after, after)
			require.NoError(t, err)
			require.Empty(t, changed)
		})
	}
}

func TestClientAuthenticationReviewCollectionFencesAndSingleEvaluation(t *testing.T) {
	for _, mode := range []string{"success", "cancel", "account", "config"} {
		t.Run(mode, func(t *testing.T) {
			f, request := rejectedAuthenticationReviewFixture(t, true)
			review := authenticationReviewAction(request.OperationID, request.Target, 1)
			require.NoError(t, os.WriteFile(filepath.Join(f.root, "block-key"), nil, 0o600))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			type result struct {
				summary clientAuthenticationReviewSummary
				err     error
			}
			done := make(chan result, 1)
			go func() { summary, err := f.w.reviewClientAuthentication(ctx, review); done <- result{summary, err} }()
			entered := filepath.Join(f.root, "key-entered")
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
		waiting:
			for {
				select {
				case <-ticker.C:
					if _, err := os.Stat(entered); err == nil {
						break waiting
					}
				case value := <-done:
					t.Fatalf("review did not enter configured key collection: %v", value.err)
				case <-ctx.Done():
					t.Fatal("review did not enter configured key collection")
				}
			}
			switch mode {
			case "cancel":
				cancel()
			case "account":
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.first))
			case "config":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
			}
			require.NoError(t, os.WriteFile(filepath.Join(f.root, "key-release"), nil, 0o600))
			var value result
			select {
			case value = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("review remained blocked after release")
			}
			if mode == "success" {
				require.NoError(t, value.err)
			} else {
				require.Error(t, value.err)
				require.Empty(t, value.summary.PreviewID)
				if mode == "cancel" {
					require.ErrorIs(t, value.err, context.Canceled)
				}
			}
			require.EqualValues(t, 1, f.puts.Load(), "review must not publish")
			requests := f.requests.Load()
			paths := []string{f.path, f.accountsPath, entered}
			infos, bodies := clientAuthenticationFiles(t, paths...)
			again, err := f.w.reviewClientAuthentication(t.Context(), review)
			require.Equal(t, value.summary, again)
			if mode == "success" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, requests, f.requests.Load(), "same review must not GET or collect again")
			if mode == "success" {
				apply := authenticationReviewApply(review, value.summary)
				_, err = f.w.applyClientAuthenticationReview(t.Context(), apply)
				require.NoError(t, err)
				_, err = f.w.applyClientAuthenticationReview(t.Context(), apply)
				require.NoError(t, err)
				require.EqualValues(t, 2, f.puts.Load())
			}
			requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
			require.Equal(t, "x", string(bodies[2]), "one configured-key evaluation per explicit review")
		})
	}
}

func TestClientAuthenticationReviewDelayedReceiverFences(t *testing.T) {
	for _, phase := range []string{"review", "apply", "acknowledge"} {
		for _, mode := range []string{"workspace", "principal", "authority"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				f, request := rejectedAuthenticationReviewFixture(t, false)
				review := authenticationReviewAction(request.OperationID, request.Target, 1)
				before := f.w.Config()
				accepted := f.w.authority.accepted
				change := func() {
					f.w.mu.Lock()
					defer f.w.mu.Unlock()
					if mode == "workspace" {
						f.w.ws.ID = "different-workspace"
						return
					}
					copy := *f.w.ws.Authority
					if mode == "principal" {
						copy.Principal = "different-principal"
					} else {
						copy.Revision++
						copy.Digest = strings.Repeat("f", 64)
					}
					f.w.ws.Authority = &copy
				}
				if phase == "review" {
					f.afterGet.Store(&change)
				}
				summary, err := f.w.reviewClientAuthentication(t.Context(), review)
				if phase == "review" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					if phase == "apply" {
						f.afterGet.Store(&change)
					} else {
						f.afterPut.Store(&change)
					}
					outcome, err := f.w.applyClientAuthenticationReview(t.Context(), authenticationReviewApply(review, summary))
					require.Error(t, err)
					require.False(t, outcome.Adopted)
					require.Equal(t, phase == "acknowledge", outcome.RemoteAcknowledged)
				}
				require.Same(t, before, f.w.Config())
				require.Equal(t, accepted, f.w.authority.accepted)
				original := f.w.authority.authenticationReceipts[request.OperationID]
				require.False(t, original.acknowledged || original.adopted)
				require.Empty(t, original.reconciledBy)
				wantPuts := int32(1)
				if phase == "acknowledge" {
					wantPuts++
				}
				require.Equal(t, wantPuts, f.puts.Load())
			})
		}
	}
}

func TestClientAuthenticationReviewNewChoiceSupersedesUnadoptedProof(t *testing.T) {
	f, request := rejectedAuthenticationReviewFixture(t, false)
	review := authenticationReviewAction(request.OperationID, request.Target, 1)
	summary, err := f.w.reviewClientAuthentication(t.Context(), review)
	require.NoError(t, err)
	oldApply := authenticationReviewApply(review, summary)
	f.putMode.Store(2)
	change := func() { f.getMode.Store(1) }
	f.afterPut.Store(&change)
	_, err = f.w.applyClientAuthenticationReview(t.Context(), oldApply)
	require.Error(t, err)
	f.afterPut.Store(nil)
	f.getMode.Store(0)
	f.putMode.Store(0)
	newReview := authenticationReviewAction(request.OperationID, request.Target, 2)
	newSummary, err := f.w.reviewClientAuthentication(t.Context(), newReview)
	require.NoError(t, err)
	require.Equal(t, summary.Receiver.Revision+1, newSummary.Receiver.Revision, "new preview explicitly observes the newer receiver")
	requests := f.requests.Load()
	_, err = f.w.applyClientAuthenticationReview(t.Context(), oldApply)
	require.ErrorIs(t, err, providerauth.ErrStale)
	require.Equal(t, requests, f.requests.Load())
	outcome, err := f.w.applyClientAuthenticationReview(t.Context(), authenticationReviewApply(newReview, newSummary))
	require.NoError(t, err)
	require.True(t, outcome.Adopted)
	require.EqualValues(t, 3, f.puts.Load())
}

func TestClientAuthenticationReviewRequestAndUnsupportedEffect(t *testing.T) {
	f, request := rejectedAuthenticationReviewFixture(t, false)
	review := authenticationReviewAction(request.OperationID, request.Target, 1)
	_, err := f.w.reviewClientAuthentication(t.Context(), review)
	require.NoError(t, err)
	changed := review
	changed.Choice = clientAuthenticationReviewChoice{Kind: "saved-logout"}
	_, err = f.w.reviewClientAuthentication(t.Context(), changed)
	require.ErrorIs(t, err, providerauth.ErrOperationConflict)
	requests := f.requests.Load()
	for _, choice := range []clientAuthenticationReviewChoice{{Kind: "unknown"}, {AccountID: "unexpected"}, {Kind: "saved-account"}, {Kind: "saved-logout", AccountID: "unexpected"}} {
		changed = authenticationReviewAction(request.OperationID, request.Target, 2)
		changed.Choice = choice
		_, err = f.w.reviewClientAuthentication(t.Context(), changed)
		require.Error(t, err)
	}
	// A future admitted checked-key operation has no OAuth account selection.
	// Its phase-one proposal recovery remains supported; this new effect does
	// not yet have a saved-state reconciliation validator.
	f.w.authority.authenticationReceipts[request.OperationID].request.accountID = ""
	for i, choice := range []clientAuthenticationReviewChoice{{}, {Kind: "saved-logout"}} {
		changed = authenticationReviewAction(request.OperationID, request.Target, uint64(i+2))
		changed.Choice = choice
		_, err = f.w.reviewClientAuthentication(t.Context(), changed)
		require.ErrorContains(t, err, "saved API-key authentication reconciliation is not supported")
	}
	require.Equal(t, requests, f.requests.Load(), "unsupported effects never read the receiver or collect")
}

func TestClientAuthenticationReviewPendingWithoutOriginalWrites(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("d", 32), Target: f.target(t), AccountID: f.second.ID}
	f.store.SetRuntimeGenerationPreparer(func(context.Context, config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		return config.RuntimeGenerationCandidate{}, errors.New("synthetic preparation refusal")
	})
	outcome, err := f.w.switchClientAuthentication(t.Context(), request)
	require.ErrorIs(t, err, providerauth.ErrMutation)
	require.False(t, clientAuthenticationChanged(outcome.Progress))
	original := f.w.authority.authenticationReceipts[request.OperationID]
	require.Equal(t, f.owner, original.owner)
	f.store.SetRuntimeGenerationPreparer(nil)
	review := authenticationReviewAction(request.OperationID, request.Target, 1)
	review.Choice = clientAuthenticationReviewChoice{Kind: "saved-account", AccountID: f.first.ID}
	summary, err := f.w.reviewClientAuthentication(t.Context(), review)
	require.NoError(t, err, "a separate explicit runtime choice may follow a zero-write admitted failure")
	f.putMode.Store(2)
	change := func() { f.getMode.Store(1) }
	f.afterPut.Store(&change)
	apply := authenticationReviewApply(review, summary)
	_, err = f.w.applyClientAuthenticationReview(t.Context(), apply)
	require.Error(t, err)
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	requests := f.requests.Load()
	require.ErrorContains(t, f.w.SetCompactMode(config.ScopeGlobal, true), "recover the saved authentication")
	f.w.authority.mu.Lock()
	err = f.w.publishClientAuthorityLocked(t.Context(), f.w.authority)
	f.w.authority.mu.Unlock()
	require.Error(t, err)
	_, err = f.w.recreateClientWorkspace(t.Context())
	require.Error(t, err)
	require.Equal(t, requests, f.requests.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
	f.afterPut.Store(nil)
	f.getMode.Store(0)
	reconciled, err := f.w.applyClientAuthenticationReview(t.Context(), apply)
	require.NoError(t, err)
	require.True(t, reconciled.Adopted)
	require.Equal(t, outcome, original.outcome)
	require.False(t, original.acknowledged || original.adopted)
	require.NoError(t, f.w.SetCompactMode(config.ScopeGlobal, true))
}
