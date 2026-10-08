package mcpclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Tool is one tool a remote server offers, in the terms the pin covers.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// surfaceVersion separates this digest from every other hash in the system, and
// from any later revision of the encoding below.
const surfaceVersion = "stelfin-mcp-surface-1"

// maxTools bounds a surface. A server offering more than this is not one a DAO
// read through before granting, which is the premise of pinning it.
const maxTools = 128

// SurfaceDigest is the hash an approved tool surface is pinned by.
//
// It is the lower-case hex SHA-256 of the following bytes, where write(s) means
// the decimal byte length of s, a colon, and then s itself:
//
//	write("stelfin-mcp-surface-1")
//	the decimal number of tools, then a colon
//	for each tool, in ascending byte order of name:
//	    write(name)
//	    write(description)
//	    write(canonical(inputSchema))
//
// canonical is not JSON. It is a tagged, length-prefixed rendering of the JSON
// value, chosen so that there is exactly one encoding of a value and no
// escaping rules to disagree about:
//
//	null            n
//	true, false     t, f
//	a number        d, then write(the number's text exactly as the server sent it)
//	a string        s, then write(the string's UTF-8 bytes, unescaped)
//	an array        a, the decimal element count, a colon, then each element
//	an object       o, the decimal member count, a colon, then for each member in
//	                ascending byte order of key: write(key), then the value
//	absent          n, the same as null
//
// What that makes equal: two schemas that differ only in whitespace, in the
// order of an object's keys, or in how a string was escaped. Those are the
// differences a server introduces by upgrading its JSON library, and none of
// them changes what a tool accepts.
//
// What it keeps different, deliberately: everything else. A number is hashed as
// its text, so 1 and 1.0 are different surfaces — this package has no business
// deciding two spellings of a number mean the same thing, and a pin that fails
// on a harmless change costs a re-approval, while one that holds across a
// harmful change costs the pin. A changed description is a changed surface too:
// the description is what the DAO read when it decided the tool was safe.
//
// Length-prefixed for the reason connector.SurfaceDigest is. Without it, a tool
// named "read" described as "write" and a tool named "readwrite" could be
// arranged to hash alike.
//
// The tools are sorted, so the order a server lists them in does not matter.
// Two tools with the same name are refused: which of them a call would reach is
// not something a pin can speak for.
func SurfaceDigest(tools []Tool) (string, error) {
	if len(tools) > maxTools {
		return "", fmt.Errorf("%w: it offers %d tools, and at most %d can be pinned",
			ErrProtocol, len(tools), maxTools)
	}

	sorted := append([]Tool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b bytes.Buffer
	write(&b, surfaceVersion)
	fmt.Fprintf(&b, "%d:", len(sorted))
	for i, tool := range sorted {
		if !validToolName(tool.Name) {
			return "", fmt.Errorf("%w: a tool has a name this will not pin", ErrProtocol)
		}
		if i > 0 && sorted[i-1].Name == tool.Name {
			return "", fmt.Errorf("%w: it offers two tools named %s", ErrProtocol, tool.Name)
		}

		schema, err := canonical(tool.InputSchema)
		if err != nil {
			return "", fmt.Errorf("%w: the input schema of %s cannot be read", ErrProtocol, tool.Name)
		}
		write(&b, tool.Name)
		write(&b, tool.Description)
		write(&b, schema)
	}

	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func write(b *bytes.Buffer, s string) {
	fmt.Fprintf(b, "%d:%s", len(s), s)
}

// validToolName reports whether a name is one this package will carry.
//
// Letters, digits, underscore, hyphen and dot. A tool's name is the one piece
// of the server's text that ends up somewhere other than a hash — in a
// capability, in a selector, in an error a person reads — so it is held to an
// alphabet in which nothing can be mistaken for anything else.
func validToolName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// canonical renders a JSON value in the encoding SurfaceDigest documents.
func canonical(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "n", nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	// So a number reaches the hash as the text the server sent, and not as
	// whatever a float64 makes of it.
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", fmt.Errorf("more than one value")
	}

	var b bytes.Buffer
	if err := writeCanonical(&b, value, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

// maxSchemaDepth bounds nesting. A schema deeper than this is not describing
// the arguments of a tool.
const maxSchemaDepth = 64

func writeCanonical(b *bytes.Buffer, value any, depth int) error {
	if depth > maxSchemaDepth {
		return fmt.Errorf("nested more than %d deep", maxSchemaDepth)
	}

	switch v := value.(type) {
	case nil:
		b.WriteByte('n')
	case bool:
		if v {
			b.WriteByte('t')
		} else {
			b.WriteByte('f')
		}
	case json.Number:
		b.WriteByte('d')
		write(b, v.String())
	case string:
		b.WriteByte('s')
		write(b, v)
	case []any:
		fmt.Fprintf(b, "a%d:", len(v))
		for _, element := range v {
			if err := writeCanonical(b, element, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		fmt.Fprintf(b, "o%d:", len(keys))
		for _, key := range keys {
			write(b, key)
			if err := writeCanonical(b, v[key], depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("a %T is not a JSON value", value)
	}
	return nil
}

// capabilityPrefix and surfacePrefix are how a pinned surface is spelt in a
// connector.Descriptor.
const (
	capabilityPrefix = "tool."
	surfacePrefix    = "surface."
)

// Capability is the capability a grant names to permit one tool.
func Capability(tool string) string { return capabilityPrefix + tool }

// ParseSelector reads a request selector.
//
// A selector is the tool's name, optionally followed by white space and the
// arguments as one JSON object:
//
//	list_invoices
//	get_rate {"pair":"XLM/USD","at":"2026-01-01"}
//
// The arguments are checked to be a single well-formed JSON object and are then
// forwarded byte for byte, apart from white space. They are never decoded into
// Go values, so a number in them is not touched by a float on its way through.
func ParseSelector(selector string) (tool string, arguments json.RawMessage, err error) {
	if len(selector) > maxSelectorBytes {
		return "", nil, fmt.Errorf("%w: it is longer than %d bytes", ErrBadSelector, maxSelectorBytes)
	}

	trimmed := strings.TrimSpace(selector)
	tool = trimmed
	rest := ""
	if i := strings.IndexAny(trimmed, " \t\r\n"); i >= 0 {
		tool, rest = trimmed[:i], strings.TrimSpace(trimmed[i:])
	}
	if !validToolName(tool) {
		return "", nil, fmt.Errorf(
			`%w. It looks like get_rate {"pair":"XLM/USD"}`, ErrBadSelector)
	}
	if rest == "" {
		return tool, json.RawMessage(`{}`), nil
	}

	// An object and nothing else. MCP arguments are named, and a selector that
	// carried an array or two values would be one whose meaning depended on
	// how the far side chose to read it.
	if rest[0] != '{' || !json.Valid([]byte(rest)) {
		return "", nil, fmt.Errorf("%w: the arguments are not one JSON object", ErrBadSelector)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(rest)); err != nil {
		return "", nil, fmt.Errorf("%w: the arguments are not one JSON object", ErrBadSelector)
	}
	return tool, compact.Bytes(), nil
}

// maxSelectorBytes bounds a selector, and so the request it becomes.
const maxSelectorBytes = 16 << 10
