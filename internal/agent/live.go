package agent

import (
	"strconv"
	"strings"

	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/ui"
)

// liveWrites renders a rolling tail of file content as write_file / edit_file
// tool calls stream their arguments in from the model, so the user sees the
// file being written in real time (last 10 lines only).
type liveWrites struct {
	ui       *ui.UI
	tools    *tool.Registry
	bufs     map[int]*strings.Builder
	names    map[int]string
	views    map[int]*ui.LiveView
	ensureNL func()
}

func newLiveWrites(u *ui.UI, tools *tool.Registry, ensureNL func()) *liveWrites {
	return &liveWrites{
		ui:       u,
		tools:    tools,
		ensureNL: ensureNL,
		bufs:     map[int]*strings.Builder{},
		names:    map[int]string{},
		views:    map[int]*ui.LiveView{},
	}
}

func (l *liveWrites) onName(index int, name string) { l.names[index] = name }

func (l *liveWrites) onArgs(index int, fragment string) {
	b := l.bufs[index]
	if b == nil {
		b = &strings.Builder{}
		l.bufs[index] = b
	}
	b.WriteString(fragment)

	var field, verb string
	switch l.names[index] {
	case "write_file":
		field, verb = "content", "writing"
	case "edit_file":
		field, verb = "new_string", "editing"
	default:
		return
	}
	content, ok := streamingField(b.String(), field)
	if !ok {
		return
	}
	view := l.views[index]
	if view == nil {
		title := verb
		if p, ok := streamingField(b.String(), "path"); ok && p != "" {
			title = verb + " " + p
		}
		if l.ensureNL != nil {
			l.ensureNL()
		}
		view = l.ui.LiveView(title, 10)
		l.views[index] = view
	}
	view.Update(content)
}

func (l *liveWrites) close() {
	for _, v := range l.views {
		v.Close()
	}
}

// streamingField extracts the best-effort value of a string field from a JSON
// object that may still be mid-stream (truncated). It returns what has been
// decoded so far, tolerating an incomplete trailing escape. ok is false only
// when the field/opening quote has not appeared yet.
func streamingField(buf, key string) (string, bool) {
	marker := `"` + key + `"`
	i := strings.Index(buf, marker)
	if i < 0 {
		return "", false
	}
	j := skipSpace(buf, i+len(marker))
	if j >= len(buf) || buf[j] != ':' {
		return "", false
	}
	j = skipSpace(buf, j+1)
	if j >= len(buf) || buf[j] != '"' {
		return "", false
	}
	j++

	var out strings.Builder
	for j < len(buf) {
		c := buf[j]
		if c == '"' {
			break // end of string value
		}
		if c != '\\' {
			out.WriteByte(c)
			j++
			continue
		}
		if j+1 >= len(buf) {
			break // incomplete escape at the stream edge
		}
		switch e := buf[j+1]; e {
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		case 'r':
			out.WriteByte('\r')
		case 'b':
			out.WriteByte('\b')
		case 'f':
			out.WriteByte('\f')
		case '/', '\\', '"':
			out.WriteByte(e)
		case 'u':
			if j+6 > len(buf) {
				return out.String(), true // incomplete \uXXXX
			}
			if r, err := strconv.ParseUint(buf[j+2:j+6], 16, 32); err == nil {
				out.WriteRune(rune(r))
			}
			j += 6
			continue
		default:
			out.WriteByte(e)
		}
		j += 2
	}
	return out.String(), true
}

func skipSpace(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}
