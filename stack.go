package argus

import (
	"bytes"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
)

const (
	sdkPackage     = "github.com/naijacloud/argus-go"
	maxFrames      = 100
	contextLines   = 5
	maxSourceBytes = 1 << 20
	maxSourceFiles = 64
)

// callerStack is the stack where the SDK was called, without the SDK's own
// frames or the runtime's.
func callerStack() *Stacktrace {
	pcs := make([]uintptr, 128)
	n := runtime.Callers(1, pcs)
	return framesFromPCs(pcs[:n])
}

func framesFromPCs(pcs []uintptr) *Stacktrace {
	if len(pcs) == 0 {
		return nil
	}
	var frames []Frame
	it := runtime.CallersFrames(pcs)
	for {
		f, more := it.Next()
		if f.Function != "" && keepFrame(f) {
			frames = append(frames, toFrame(f))
		}
		if !more {
			break
		}
	}
	if len(frames) == 0 {
		return nil
	}
	// runtime order is newest first; Argus wants oldest first.
	for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
		frames[i], frames[j] = frames[j], frames[i]
	}
	if len(frames) > maxFrames {
		frames = frames[len(frames)-maxFrames:]
	}
	return &Stacktrace{Frames: frames}
}

func keepFrame(f runtime.Frame) bool {
	mod, _ := splitFunction(f.Function)
	if mod == "runtime" || strings.HasPrefix(mod, "runtime/") {
		return false
	}
	// The SDK's own frames, but not its tests or examples.
	if mod == sdkPackage && !strings.HasSuffix(f.File, "_test.go") {
		return false
	}
	return true
}

// splitFunction splits "github.com/a/b/pkg.(*T).M" into the package path and
// the function within it.
func splitFunction(name string) (module, function string) {
	slash := strings.LastIndexByte(name, '/')
	dot := strings.IndexByte(name[slash+1:], '.')
	if dot < 0 {
		return "", name
	}
	return name[:slash+1+dot], name[slash+1+dot+1:]
}

func isStdlib(module string) bool {
	if module == "main" || module == "" {
		return false
	}
	first := module
	if i := strings.IndexByte(module, '/'); i >= 0 {
		first = module[:i]
	}
	return !strings.Contains(first, ".")
}

func toFrame(f runtime.Frame) Frame {
	mod, fn := splitFunction(f.Function)
	base := f.File[strings.LastIndexByte(f.File, '/')+1:]
	filename := base
	if mod != "" && mod != "main" {
		filename = mod + "/" + base
	}
	inApp := !isStdlib(mod) &&
		!strings.Contains(f.File, "/pkg/mod/") &&
		!strings.Contains(f.File, "/vendor/") &&
		!strings.HasPrefix(mod, sdkPackage+"/")
	if mod == sdkPackage && !strings.HasSuffix(f.File, "_test.go") {
		inApp = false
	}
	fr := Frame{Filename: filename, AbsPath: f.File, Function: fn, Module: mod, Lineno: f.Line, InApp: inApp}
	if inApp {
		addSourceContext(&fr)
	}
	return fr
}

var sources struct {
	sync.Mutex
	files map[string][][]byte
}

func addSourceContext(fr *Frame) {
	lines := sourceLines(fr.AbsPath)
	i := fr.Lineno - 1
	if lines == nil || i < 0 || i >= len(lines) {
		return
	}
	fr.ContextLine = string(lines[i])
	for j := max(0, i-contextLines); j < i; j++ {
		fr.PreContext = append(fr.PreContext, string(lines[j]))
	}
	for j := i + 1; j < len(lines) && j <= i+contextLines; j++ {
		fr.PostContext = append(fr.PostContext, string(lines[j]))
	}
}

// sourceLines reads a source file once (most deployed binaries have none,
// which is fine: the frame just has no context lines).
func sourceLines(path string) [][]byte {
	sources.Lock()
	defer sources.Unlock()
	if sources.files == nil {
		sources.files = map[string][][]byte{}
	}
	if l, ok := sources.files[path]; ok {
		return l
	}
	var lines [][]byte
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Size() <= maxSourceBytes {
		if b, err := os.ReadFile(path); err == nil {
			lines = bytes.Split(b, []byte("\n"))
			for i := range lines {
				lines[i] = bytes.TrimRight(lines[i], "\r")
			}
		}
	}
	if len(sources.files) >= maxSourceFiles {
		sources.files = map[string][][]byte{}
	}
	sources.files[path] = lines
	return lines
}

// errorStack returns the program counters an error recorded itself, for
// errors made by github.com/pkg/errors (StackTrace()) or go-errors (Callers()),
// read by reflection so neither is a dependency.
func errorStack(err error) (pcs []uintptr) {
	defer func() {
		if recover() != nil {
			pcs = nil
		}
	}()
	v := reflect.ValueOf(err)
	for _, name := range []string{"StackTrace", "Callers"} {
		m := v.MethodByName(name)
		if !m.IsValid() || m.Type().NumIn() != 0 || m.Type().NumOut() != 1 {
			continue
		}
		out := m.Call(nil)[0]
		if out.Kind() != reflect.Slice || out.Type().Elem().Kind() != reflect.Uintptr {
			continue
		}
		pcs = make([]uintptr, out.Len())
		for i := range pcs {
			pcs[i] = uintptr(out.Index(i).Uint())
		}
		return pcs
	}
	return nil
}
