package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/message"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/session"
)

type RemoteCompactor interface {
	Compact(context.Context, fantasy.Call) (*codexresponses.CompactionResult, error)
}

type remoteCompactionCapture struct {
	result *codexresponses.CompactionResult
}

type remoteCompactionAgentModel struct {
	fantasy.LanguageModel
	admitted *Model
	capture  *remoteCompactionCapture
}

func (m remoteCompactionAgentModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	if m.admitted == nil || m.admitted.Compactor == nil {
		return nil, fmt.Errorf("remote compaction executor is unavailable")
	}
	result, err := m.admitted.Compactor.Compact(ctx, call)
	if err != nil {
		return nil, err
	}
	if result == nil || result.History == nil {
		return nil, fmt.Errorf("remote compaction returned no provider checkpoint")
	}
	m.capture.result = result
	return &fantasy.Response{
		FinishReason: fantasy.FinishReasonStop,
		Usage:        result.Usage,
	}, nil
}

func (a *sessionAgent) generateRemoteCompaction(
	ctx context.Context,
	sessionID string,
	currentSession session.Session,
	messages []fantasy.Message,
	instructions fantasy.Instructions,
	providerOptions fantasy.ProviderOptions,
	onAuthRefresh func(context.Context, *fantasy.ProviderError) error,
	runtime InstalledRuntime,
	admitted *Model,
	initialPolicy manifest.CompactionPolicy,
	initialRetry manifest.RetryPolicy,
	initialMetadata manifest.MetadataContract,
) (*codexresponses.CompactionResult, error) {
	if admitted == nil || admitted.Compactor == nil {
		return nil, fmt.Errorf("remote compaction executor is unavailable")
	}
	if admitted.CompactionRetry == nil {
		return nil, fmt.Errorf("remote compaction retry policy is unavailable")
	}
	capture := &remoteCompactionCapture{}
	modelProvider := func() fantasy.LanguageModel {
		return remoteCompactionAgentModel{
			LanguageModel: admitted.Model,
			admitted:      admitted,
			capture:       capture,
		}
	}
	retryModel := *admitted
	retryModel.Retry = admitted.CompactionRetry
	compactionAgent := fantasy.NewAgent(
		modelProvider(),
		fantasy.WithInstructions(instructions, admitted.InstructionPolicy),
		fantasy.WithTools(runtime.Tools...),
		fantasy.WithProviderOptions(admitted.ProviderOptions),
		fantasy.WithUserAgent(userAgent),
	)
	_, err := compactionAgent.Generate(ctx, fantasy.AgentCall{
		Prompt:          "compact prepared conversation",
		Messages:        messages,
		Headers:         sessionHeaders(sessionID, "compaction"),
		ProviderOptions: providerOptions,
		MaxRetries:      modelMaxRetries(retryModel),
		OnAuthRefresh: modelAuthRefresh(
			retryModel,
			refreshAdmittedModelValidated(
				admitted,
				func() Model { return a.Runtime().LargeModel },
				onAuthRefresh,
				func(refreshed Model) error {
					return validateRemoteCompactionAdmission(initialPolicy, initialRetry, initialMetadata, refreshed)
				},
			),
		),
		ModelProvider: modelProvider,
		PrepareStep: func(callContext context.Context, _ fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = replaceSystemInstructions(messages, instructions, admitted.InstructionPolicy)
			for i := range prepared.Messages {
				prepared.Messages[i].ProviderOptions = nil
			}
			prepared.Messages, err = a.workaroundProviderMediaLimitations(prepared.Messages, *admitted)
			if err != nil {
				return callContext, prepared, err
			}
			selectedTools := toolsForSessionMode(currentSession.Mode, runtime.Tools, runtime.PlanModeTools)
			prepared.Tools = codexBoundedTools(selectedTools, *admitted)
			return callContext, prepared, nil
		},
	})
	if err != nil {
		return nil, err
	}
	if capture.result == nil {
		return nil, fmt.Errorf("remote compaction completed without a provider checkpoint")
	}
	return capture.result, nil
}

func cloneRetryPolicy(policy manifest.RetryPolicy) manifest.RetryPolicy {
	policy.Statuses = slices.Clone(policy.Statuses)
	policy.Codes = slices.Clone(policy.Codes)
	return policy
}

func compactionMetadataContract(contracts []manifest.MetadataContract, namespace string) (manifest.MetadataContract, error) {
	if namespace == "" {
		return manifest.MetadataContract{}, fmt.Errorf("remote compaction metadata namespace is unavailable")
	}
	var contract *manifest.MetadataContract
	for i := range contracts {
		candidate := &contracts[i]
		if candidate.Namespace != namespace || candidate.Scope != string(message.ProviderMetadataScopeCompaction) {
			continue
		}
		if contract != nil {
			return manifest.MetadataContract{}, fmt.Errorf("remote compaction metadata namespace %q is declared more than once", namespace)
		}
		contract = candidate
	}
	if contract == nil {
		return manifest.MetadataContract{}, fmt.Errorf("remote compaction metadata namespace %q has no compaction contract", namespace)
	}
	if !contract.RequiredForReplay {
		return manifest.MetadataContract{}, fmt.Errorf("remote compaction metadata namespace %q is not required for replay", namespace)
	}
	schemaData, err := json.Marshal(contract.Schema)
	if err != nil {
		return manifest.MetadataContract{}, fmt.Errorf("remote compaction metadata namespace %q schema: %w", namespace, err)
	}
	cloned := *contract
	cloned.Schema = nil
	if err := json.Unmarshal(schemaData, &cloned.Schema); err != nil {
		return manifest.MetadataContract{}, fmt.Errorf("remote compaction metadata namespace %q schema: %w", namespace, err)
	}
	return cloned, nil
}

func validateRemoteCompactionAdmission(
	initialPolicy manifest.CompactionPolicy,
	initialRetry manifest.RetryPolicy,
	initialMetadata manifest.MetadataContract,
	current Model,
) error {
	if current.Compaction == nil || *current.Compaction != initialPolicy {
		return fmt.Errorf("remote compaction declaration changed during authentication refresh")
	}
	if current.Compactor == nil {
		return fmt.Errorf("remote compaction executor changed during authentication refresh")
	}
	if current.CompactionRetry == nil || !reflect.DeepEqual(*current.CompactionRetry, initialRetry) {
		return fmt.Errorf("remote compaction retry policy changed during authentication refresh")
	}
	currentMetadata, err := compactionMetadataContract(current.Metadata, initialPolicy.MetadataNamespace)
	if err != nil {
		return fmt.Errorf("remote compaction metadata changed during authentication refresh: %w", err)
	}
	if !reflect.DeepEqual(currentMetadata, initialMetadata) {
		return fmt.Errorf("remote compaction metadata changed during authentication refresh")
	}
	return nil
}

func compactionMetadata(
	contracts []manifest.MetadataContract,
	namespace string,
	history *codexresponses.CompactedHistory,
) (message.ProviderMetadata, error) {
	contract, err := compactionMetadataContract(contracts, namespace)
	if err != nil {
		return nil, err
	}
	compiled, err := manifest.CompileMetadataContracts([]manifest.MetadataContract{contract})
	if err != nil {
		return nil, err
	}
	if err := manifest.ValidateMetadataValue(namespace, compiled[namespace], history); err != nil {
		return nil, fmt.Errorf("remote compaction metadata %q: %w", namespace, err)
	}
	envelope, err := message.NewProviderMetadataValue(
		namespace,
		contract.Version,
		message.ProviderMetadataScopeCompaction,
		history,
	)
	if err != nil {
		return nil, err
	}
	return message.ProviderMetadata{envelope}, nil
}
