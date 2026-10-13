package telegram

import (
	"reflect"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	for _, tc := range []struct {
		name, in, out string
		ents          []MessageEntity
	}{
		{"plain", "Hello *world* <b>", "Hello *world* <b>", nil},
		{"inline", "run `ls -l` now", "run ls -l now", []MessageEntity{{Type: "code", Offset: 4, Length: 5}}},
		{"unclosed inline", "a ` b", "a ` b", nil},
		{"fence", "Here:\n```go\nfmt.Println(1)\n```\nDone.", "Here:\nfmt.Println(1)\nDone.",
			[]MessageEntity{{Type: "pre", Offset: 6, Length: 14, Language: "go"}}},
		{"unclosed fence while streaming", "x\n```\nab", "x\nab", []MessageEntity{{Type: "pre", Offset: 2, Length: 2}}},
		{"utf16 offsets", "😀 `x`", "😀 x", []MessageEntity{{Type: "code", Offset: 3, Length: 1}}},
	} {
		out, ents := Render(tc.in)
		if out != tc.out || !reflect.DeepEqual(ents, tc.ents) {
			t.Errorf("%s: Render(%q) = %q, %+v; want %q, %+v", tc.name, tc.in, out, ents, tc.out, tc.ents)
		}
	}
}

func TestSplitText(t *testing.T) {
	long := strings.Repeat("a", 30) + "\n" + strings.Repeat("b", 30)
	parts := splitText(long, 40)
	if len(parts) != 2 || parts[0] != strings.Repeat("a", 30)+"\n" || strings.Join(parts, "") != long {
		t.Fatalf("parts = %q", parts)
	}
	emoji := strings.Repeat("😀", 5) // 10 UTF-16 units
	parts = splitText(emoji, 4)
	if len(parts) != 3 || parts[0] != "😀😀" || strings.Join(parts, "") != emoji {
		t.Fatalf("emoji parts = %q", parts)
	}
	if p := splitText("", 10); len(p) != 1 || p[0] != "" {
		t.Fatalf("empty = %q", p)
	}
	text, ents := withHeader("Routine x", "`a`", []MessageEntity{{Type: "code", Offset: 0, Length: 1}})
	if text != "Routine x\n\n`a`" || ents[1].Offset != 11 || ents[0].Type != "bold" {
		t.Fatalf("withHeader = %q %+v", text, ents)
	}
}
