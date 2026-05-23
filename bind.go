package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
)

type bindingEntry struct {
	fn         reflect.Value
	numIn      int
	isVariadic bool
	inTypes    []reflect.Type
	hasReturn  bool
	hasError   bool
}

var errorType = reflect.TypeOf((*error)(nil)).Elem()

func newBindingEntry(f interface{}) (*bindingEntry, error) {
	v := reflect.ValueOf(f)
	if v.Kind() != reflect.Func {
		return nil, errors.New("only functions can be bound")
	}
	t := v.Type()
	numOut := t.NumOut()
	if numOut > 2 {
		return nil, errors.New("function may only return a value or a value+error")
	}

	numIn := t.NumIn()
	inTypes := make([]reflect.Type, numIn)
	for i := 0; i < numIn; i++ {
		inTypes[i] = t.In(i)
	}

	e := &bindingEntry{
		fn:         v,
		numIn:      numIn,
		isVariadic: t.IsVariadic(),
		inTypes:    inTypes,
		hasReturn:  numOut > 0,
	}

	if numOut == 1 {
		e.hasError = t.Out(0).Implements(errorType)
	} else if numOut == 2 {
		if !t.Out(1).Implements(errorType) {
			return nil, errors.New("second return value must be an error")
		}
		e.hasError = true // 第二个返回值固定是 error
	}

	return e, nil
}

func (e *bindingEntry) call(params []json.RawMessage) (interface{}, error) {
	if e.isVariadic {
		if len(params) < e.numIn-1 {
			return nil, errors.New("function arguments mismatch")
		}
	} else {
		if len(params) != e.numIn {
			return nil, errors.New("function arguments mismatch")
		}
	}

	args := make([]reflect.Value, len(params))
	for i, param := range params {
		var argType reflect.Type
		if e.isVariadic && i >= e.numIn-1 {
			// variadic：取切片元素类型
			argType = e.inTypes[e.numIn-1].Elem()
		} else {
			argType = e.inTypes[i]
		}
		arg := reflect.New(argType)
		if err := json.Unmarshal(param, arg.Interface()); err != nil {
			return nil, err
		}
		args[i] = arg.Elem()
	}

	res := e.fn.Call(args)

	switch len(res) {
	case 0:
		return nil, nil
	case 1:
		if e.hasError {
			if !res[0].IsNil() {
				return nil, res[0].Interface().(error)
			}
			return nil, nil
		}
		return res[0].Interface(), nil
	case 2:
		if res[1].IsNil() {
			return res[0].Interface(), nil
		}
		return res[0].Interface(), res[1].Interface().(error)
	default:
		return nil, errors.New("unexpected number of return values")
	}
}

func (w *webview) Bind(name string, f interface{}) error {
	entry, err := newBindingEntry(f)
	if err != nil {
		return err
	}

	w.mu.Lock()
	w.bindings[name] = entry
	w.mu.Unlock()

	// JS 注入：与原实现完全兼容，无行为变化
	w.Init("(function() { var name = " + jsString(name) + ";" + `
		var RPC = window._rpc = (window._rpc || {nextSeq: 1});
		window[name] = function() {
			var seq = RPC.nextSeq++;
			var promise = new Promise(function(resolve, reject) {
				RPC[seq] = { resolve: resolve, reject: reject };
			});
			window.external.invoke(JSON.stringify({
				id: seq,
				method: name,
				params: Array.prototype.slice.call(arguments),
			}));
			return promise;
		};
	})()`)

	return nil
}

func (w *webview) callbinding(d rpcMessage) (interface{}, error) {
	w.mu.Lock()
	entry, ok := w.bindings[d.Method]
	w.mu.Unlock()

	if !ok {
		return nil, nil
	}

	e, ok := entry.(*bindingEntry)
	if !ok {
		return nil, errors.New("internal: invalid binding entry type")
	}

	return e.call(d.Params)
}

func (w *webview) msgcb(msg string) {
	d := rpcMessage{}
	if err := json.Unmarshal([]byte(msg), &d); err != nil {
		return
	}

	id := strconv.Itoa(d.ID)
	res, err := w.callbinding(d)

	if err != nil {
		errStr := jsString(err.Error())
		js := buildRPCReject(id, errStr)
		w.Dispatch(func() { w.Eval(js) })
		return
	}

	b, err := json.Marshal(res)
	if err != nil {
		errStr := jsString(err.Error())
		js := buildRPCReject(id, errStr)
		w.Dispatch(func() { w.Eval(js) })
		return
	}

	js := buildRPCResolve(id, string(b))
	w.Dispatch(func() { w.Eval(js) })
}

func buildRPCResolve(id, result string) string {
	var b strings.Builder
	b.Grow(32 + len(id)*2 + len(result))
	b.WriteString("window._rpc[")
	b.WriteString(id)
	b.WriteString("].resolve(")
	b.WriteString(result)
	b.WriteString(");window._rpc[")
	b.WriteString(id)
	b.WriteString("]=undefined")
	return b.String()
}

func buildRPCReject(id, errStr string) string {
	var b strings.Builder
	b.Grow(32 + len(id)*2 + len(errStr))
	b.WriteString("window._rpc[")
	b.WriteString(id)
	b.WriteString("].reject(")
	b.WriteString(errStr)
	b.WriteString(");window._rpc[")
	b.WriteString(id)
	b.WriteString("]=undefined")
	return b.String()
}
