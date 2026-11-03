package subagentscript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dop251/goja"
)

type Result struct {
	Tasks  []any `json:"tasks,omitempty"`
	Result any   `json:"result"`
	Output []any `json:"output,omitempty"`
}

func Run(ctx context.Context, service Caller, sessionID, parentSessionID, source string) (Result, error) {
	var result Result
	if err := ctx.Err(); err != nil {
		return result, err
	}
	vm := goja.New()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-done:
		}
	}()
	toValue := func(value any) goja.Value {
		data, err := json.Marshal(value)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(decoded)
	}
	invoke := func(request Request) goja.Value {
		if err := request.Validate(); err != nil {
			panic(vm.NewGoError(err))
		}
		value, err := service.Call(ctx, request)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		if request.Action == "start" || request.Action == "continue" {
			result.Tasks = append(result.Tasks, value)
		}
		return toValue(value)
	}
	decode := func(value goja.Value, target any) {
		if goja.IsUndefined(value) || goja.IsNull(value) {
			panic(vm.NewTypeError("an options object is required"))
		}
		data, err := json.Marshal(value.Export())
		if err == nil {
			err = Decode(data, target)
		}
		if err != nil {
			panic(vm.NewGoError(err))
		}
	}
	methods := map[string]any{}
	for _, action := range []string{"list", "types", "output", "wait", "stop"} {
		methods[action] = func(call goja.FunctionCall) goja.Value {
			arguments := 1
			if action == "list" || action == "types" {
				arguments = 0
			}
			if len(call.Arguments) != arguments && !(action == "wait" && len(call.Arguments) == 2) {
				panic(vm.NewTypeError("invalid arguments for subagents." + action))
			}
			request := Request{Action: action}
			if action != "list" && action != "types" {
				if id, ok := call.Argument(0).Export().(string); ok {
					request.TaskID = id
				} else {
					panic(vm.NewTypeError("task ID must be a string"))
				}
			}
			if action == "wait" && !goja.IsUndefined(call.Argument(1)) {
				var options struct {
					TimeoutMS *int64 `json:"timeout_ms,omitempty"`
				}
				decode(call.Argument(1), &options)
				request.TimeoutMS = options.TimeoutMS
			}
			return invoke(request)
		}
	}
	methods["start"] = func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) != 1 {
			panic(vm.NewTypeError("start requires one options object"))
		}
		var options struct {
			Prompt       string `json:"prompt"`
			SubagentType string `json:"subagent_type,omitempty"`
		}
		decode(call.Argument(0), &options)
		return invoke(Request{Action: "start", Prompt: options.Prompt, SubagentType: options.SubagentType})
	}
	methods["continue"] = func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) != 2 {
			panic(vm.NewTypeError("continue requires a task ID and prompt"))
		}
		id, idOK := call.Argument(0).Export().(string)
		prompt, promptOK := call.Argument(1).Export().(string)
		if !idOK || !promptOK {
			panic(vm.NewTypeError("continue requires a task ID and prompt string"))
		}
		return invoke(Request{Action: "continue", TaskID: id, Prompt: prompt})
	}
	api := vm.NewObject()
	for name, method := range methods {
		if err := api.Set(name, method); err != nil {
			return result, err
		}
	}
	if err := vm.Set("subagents", api); err != nil {
		return result, err
	}
	environment := vm.NewObject()
	for name, value := range map[string]string{SessionEnv: sessionID, ParentEnv: parentSessionID} {
		if err := environment.Set(name, value); err != nil {
			return result, err
		}
	}
	if err := vm.Set("env", environment); err != nil {
		return result, err
	}
	if err := vm.Set("print", func(call goja.FunctionCall) goja.Value {
		for _, value := range call.Arguments {
			result.Output = append(result.Output, toValue(value.Export()).Export())
		}
		return goja.Undefined()
	}); err != nil {
		return result, err
	}
	value, err := vm.RunString("Object.freeze(env); Object.freeze(subagents); (function(){\n" + source + "\n})()")
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fmt.Errorf("subagent script: %w", err)
	}
	if _, ok := value.Export().(*goja.Promise); ok {
		return result, errors.New("subagent scripts use synchronous functions; return a value, not a promise")
	}
	result.Result = value.Export()
	if _, err := json.Marshal(result.Result); err != nil {
		result.Result = nil
		return result, fmt.Errorf("script return value must be JSON: %w", err)
	}
	return result, nil
}
