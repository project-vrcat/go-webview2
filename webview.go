//go:build windows
// +build windows

package webview2

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"github.com/project-vrcat/go-webview2/internal/w32"
	"github.com/project-vrcat/go-webview2/pkg/edge"

	"golang.org/x/sys/windows"
)

var (
	windowContext     = map[uintptr]interface{}{}
	windowContextSync sync.RWMutex
)

func getWindowContext(wnd uintptr) interface{} {
	windowContextSync.RLock()
	defer windowContextSync.RUnlock()
	return windowContext[wnd]
}

func setWindowContext(wnd uintptr, data interface{}) {
	windowContextSync.Lock()
	defer windowContextSync.Unlock()
	windowContext[wnd] = data
}

type browser interface {
	Embed(hwnd uintptr) bool
	Resize()
	Navigate(url string)
	Init(script string)
	Eval(script string)
}

type WebView struct {
	HWND       uintptr
	mainthread uintptr
	Browser    *edge.Chromium
	maxsz      w32.Point
	minsz      w32.Point
	m          sync.Mutex
	bindings   map[string]interface{}
	dispatchq  []func()
}

// New creates a new webview in a new window.
func New(debug bool, userDataFolder ...string) *WebView {
	return NewWindow(debug, nil, userDataFolder...)
}

// NewWindow creates a new webview using an existing window.
func NewWindow(debug bool, window unsafe.Pointer, userDataFolder ...string) *WebView {
	w := &WebView{}
	w.bindings = map[string]interface{}{}

	chromium := edge.NewChromium()
	chromium.MessageCallback = w.msgcb
	chromium.Debug = debug

	w.Browser = chromium
	w.mainthread, _, _ = w32.Kernel32GetCurrentThreadID.Call()
	if !w.Create(debug, window, userDataFolder...) {
		return nil
	}
	return w
}

type rpcMessage struct {
	ID     int               `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func jsString(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

func (w *WebView) msgcb(msg string) {
	d := rpcMessage{}
	if err := json.Unmarshal([]byte(msg), &d); err != nil {
		log.Printf("invalid RPC message: %v", err)
		return
	}

	id := strconv.Itoa(d.ID)
	if res, err := w.callbinding(d); err != nil {
		w.Dispatch(func() {
			w.Eval("window._rpc[" + id + "].reject(" + jsString(err.Error()) + "); window._rpc[" + id + "] = undefined")
		})
	} else if b, err := json.Marshal(res); err != nil {
		w.Dispatch(func() {
			w.Eval("window._rpc[" + id + "].reject(" + jsString(err.Error()) + "); window._rpc[" + id + "] = undefined")
		})
	} else {
		w.Dispatch(func() {
			w.Eval("window._rpc[" + id + "].resolve(" + string(b) + "); window._rpc[" + id + "] = undefined")
		})
	}
}

func (w *WebView) callbinding(d rpcMessage) (interface{}, error) {
	w.m.Lock()
	f, ok := w.bindings[d.Method]
	w.m.Unlock()
	if !ok {
		return nil, nil
	}

	v := reflect.ValueOf(f)
	isVariadic := v.Type().IsVariadic()
	numIn := v.Type().NumIn()
	if (isVariadic && len(d.Params) < numIn-1) || (!isVariadic && len(d.Params) != numIn) {
		return nil, errors.New("function arguments mismatch")
	}
	args := []reflect.Value{}
	for i := range d.Params {
		var arg reflect.Value
		if isVariadic && i >= numIn-1 {
			arg = reflect.New(v.Type().In(numIn - 1).Elem())
		} else {
			arg = reflect.New(v.Type().In(i))
		}
		if err := json.Unmarshal(d.Params[i], arg.Interface()); err != nil {
			return nil, err
		}
		args = append(args, arg.Elem())
	}

	errorType := reflect.TypeOf((*error)(nil)).Elem()
	res := v.Call(args)
	switch len(res) {
	case 0:
		// No results from the function, just return nil
		return nil, nil

	case 1:
		// One result may be a value, or an error
		if res[0].Type().Implements(errorType) {
			if res[0].Interface() != nil {
				return nil, res[0].Interface().(error)
			}
			return nil, nil
		}
		return res[0].Interface(), nil

	case 2:
		// Two results: first one is value, second is error
		if !res[1].Type().Implements(errorType) {
			return nil, errors.New("second return value must be an error")
		}
		if res[1].Interface() == nil {
			return res[0].Interface(), nil
		}
		return res[0].Interface(), res[1].Interface().(error)

	default:
		return nil, errors.New("unexpected number of return values")
	}
}

func wndproc(hwnd, msg, wp, lp uintptr) uintptr {
	if w, ok := getWindowContext(hwnd).(*WebView); ok {
		switch msg {
		case w32.WMSize:
			w.Browser.Resize()
		case w32.WMClose:
			w32.User32DestroyWindow.Call(hwnd)
		case w32.WMDestroy:
			w.Terminate()
		case w32.WMGetMinMaxInfo:
			lpmmi := (*w32.MinMaxInfo)(unsafe.Pointer(lp))
			if w.maxsz.X > 0 && w.maxsz.Y > 0 {
				lpmmi.PtMaxSize = w.maxsz
				lpmmi.PtMaxTrackSize = w.maxsz
			}
			if w.minsz.X > 0 && w.minsz.Y > 0 {
				lpmmi.PtMinTrackSize = w.minsz
			}
		default:
			r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
			return r
		}
		return 0
	}
	r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func (w *WebView) Create(debug bool, window unsafe.Pointer, userDataFolder ...string) bool {
	var hinstance windows.Handle
	windows.GetModuleHandleEx(0, nil, &hinstance)

	icon := w32.ExtractIcon(os.Args[0], 0)

	className, _ := windows.UTF16PtrFromString("webview")
	wc := w32.WndClassExW{
		CbSize:        uint32(unsafe.Sizeof(w32.WndClassExW{})),
		HInstance:     hinstance,
		LpszClassName: className,
		HIcon:         windows.Handle(icon),
		HIconSm:       windows.Handle(icon),
		LpfnWndProc:   windows.NewCallback(wndproc),
	}
	w32.User32RegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	windowName, _ := windows.UTF16PtrFromString("")
	w.HWND, _, _ = w32.User32CreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		0xCF0000,   // WS_OVERLAPPEDWINDOW
		0x80000000, // CW_USEDEFAULT
		0x80000000, // CW_USEDEFAULT
		640,
		480,
		0,
		0,
		uintptr(hinstance),
		0,
	)
	setWindowContext(w.HWND, w)

	w32.User32ShowWindow.Call(w.HWND, w32.SWShow)
	w32.User32UpdateWindow.Call(w.HWND)
	w32.User32SetFocus.Call(w.HWND)

	if !w.Browser.Embed(w.HWND, userDataFolder...) {
		return false
	}
	w.Browser.Resize()
	return true
}

func (w *WebView) Destroy() {
}

func (w *WebView) Run() {
	var msg w32.Msg
	for {
		w32.User32GetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)),
			0,
			0,
			0,
		)
		if msg.Message == w32.WMApp {
			w.m.Lock()
			q := append([]func(){}, w.dispatchq...)
			w.dispatchq = []func(){}
			w.m.Unlock()
			for _, v := range q {
				v()
			}
		} else if msg.Message == w32.WMQuit {
			return
		}
		w32.User32TranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		w32.User32DispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (w *WebView) Terminate() {
	w32.User32PostQuitMessage.Call(0)
}

func (w *WebView) Window() unsafe.Pointer {
	return unsafe.Pointer(w.HWND)
}

func (w *WebView) Navigate(url string) {
	w.Browser.Navigate(url)
}

func (w *WebView) SetTitle(title string) {
	_title, err := windows.UTF16FromString(title)
	if err != nil {
		_title, _ = windows.UTF16FromString("")
	}
	w32.User32SetWindowTextW.Call(w.HWND, uintptr(unsafe.Pointer(&_title[0])))
}

func (w *WebView) SetSize(width int, height int, hints Hint) {
	index := w32.GWLStyle
	style, _, _ := w32.User32GetWindowLongPtrW.Call(w.HWND, uintptr(index))
	if hints == HintFixed {
		style &^= (w32.WSThickFrame | w32.WSMaximizeBox)
	} else {
		style |= (w32.WSThickFrame | w32.WSMaximizeBox)
	}
	w32.User32SetWindowLongPtrW.Call(w.HWND, uintptr(index), style)

	if hints == HintMax {
		w.maxsz.X = int32(width)
		w.maxsz.Y = int32(height)
	} else if hints == HintMin {
		w.minsz.X = int32(width)
		w.minsz.Y = int32(height)
	} else if hints == HintCenter {
		scrWidth, _, _ := w32.User32GetSystemMetrics.Call(w32.SystemMetricsCxScreen)
		scrHeight, _, _ := w32.User32GetSystemMetrics.Call(w32.SystemMetricsCyScreen)
		rect := new(w32.Rect)
		ret, _, _ := syscall.Syscall(w32.User32GetWindowRect.Addr(), 2,
			w.HWND,
			uintptr(unsafe.Pointer(rect)),
			0)
		if ret == 0 {
			return
		}
		rect.Left = int32((int(scrWidth) - width) / 2)
		rect.Top = int32((int(scrHeight) - height) / 2)
		_, _, _ = w32.User32MoveWindow.Call(w.HWND,
			uintptr(rect.Left), uintptr(rect.Top),
			uintptr(width), uintptr(height),
			1)
		w.Browser.Resize()
	} else {
		r := w32.Rect{}
		r.Left = 0
		r.Top = 0
		r.Right = int32(width)
		r.Bottom = int32(height)
		w32.User32AdjustWindowRect.Call(uintptr(unsafe.Pointer(&r)), w32.WSOverlappedWindow, 0)
		w32.User32SetWindowPos.Call(
			w.HWND, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top),
			w32.SWPNoZOrder|w32.SWPNoActivate|w32.SWPNoMove|w32.SWPFrameChanged)
		w.Browser.Resize()
	}
}

func (w *WebView) Init(js string) {
	w.Browser.Init(js)
}

func (w *WebView) Eval(js string) {
	w.Browser.Eval(js)
}

func (w *WebView) Dispatch(f func()) {
	w.m.Lock()
	w.dispatchq = append(w.dispatchq, f)
	w.m.Unlock()
	w32.User32PostThreadMessageW.Call(w.mainthread, w32.WMApp, 0, 0)
}

func (w *WebView) Bind(name string, f interface{}) error {
	v := reflect.ValueOf(f)
	if v.Kind() != reflect.Func {
		return errors.New("only functions can be bound")
	}
	if n := v.Type().NumOut(); n > 2 {
		return errors.New("function may only return a value or a value+error")
	}
	w.m.Lock()
	w.bindings[name] = f
	w.m.Unlock()

	w.Init("(function() { var name = " + jsString(name) + ";" + `
		var RPC = window._rpc = (window._rpc || {nextSeq: 1});
		window[name] = function() {
		  var seq = RPC.nextSeq++;
		  var promise = new Promise(function(resolve, reject) {
			RPC[seq] = {
			  resolve: resolve,
			  reject: reject,
			};
		  });
		  window.external.invoke(JSON.stringify({
			id: seq,
			method: name,
			params: Array.prototype.slice.call(arguments),
		  }));
		  return promise;
		}
	})()`)

	return nil
}
