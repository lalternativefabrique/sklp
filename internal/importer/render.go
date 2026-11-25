package importer

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func renderDevStack(st *devStack, sources []string) []byte {
	root := mapping()
	manual := ManualCount(st.allNotes())
	header := []string{
		"Imported from " + strings.Join(sources, " + ") + " by `sklp init`.",
		fmt.Sprintf("%d manual action(s) left: grep TODO(import). Lines marked import: record a translation choice.", manual),
	}
	for _, n := range st.notes {
		header = append(header, n.comment())
	}

	put(root, "version", scalarInt(1))
	root.Content[0].HeadComment = strings.Join(header, "\n")
	put(root, "kind", scalar("devstack"))
	put(root, "name", scalar("dev"))
	if st.allowHost {
		put(root, "allowHost", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	}
	if len(st.envFiles) > 0 {
		put(root, "env_files", seq(st.envFiles))
	}

	services := mapping()
	for _, s := range st.services {
		key := scalar(s.name)
		var comments []string
		for _, n := range s.notes {
			comments = append(comments, n.comment())
		}
		key.HeadComment = strings.Join(comments, "\n")
		services.Content = append(services.Content, key, renderService(s))
	}
	put(root, "services", services)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}})
	_ = enc.Close()
	return spaceServices(buf.Bytes())
}

func renderService(s *devService) *yaml.Node {
	m := mapping()
	if s.image != "" {
		put(m, "image", scalar(s.image))
		put(m, "network", scalar(s.network))
	} else {
		put(m, "run", scalar(s.run))
		if s.workdir != "" {
			put(m, "workdir", scalar(s.workdir))
		}
	}
	if s.port > 0 {
		put(m, "port", scalarInt(s.port))
	}
	if len(s.command) > 0 {
		put(m, "command", flowSeq(s.command))
	}
	if len(s.env) > 0 {
		env := mapping()
		keys := make([]string, 0, len(s.env))
		for k := range s.env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env.Content = append(env.Content, scalar(k), stringScalar(s.env[k]))
		}
		put(m, "env", env)
	}
	if len(s.volumes) > 0 {
		put(m, "volumes", seq(s.volumes))
	}
	if s.oneshot {
		put(m, "oneshot", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	}
	if s.optional {
		put(m, "optional", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
	}
	if len(s.dependsOn) > 0 {
		put(m, "depends_on", seq(s.dependsOn))
	}
	if r := s.ready; r != nil {
		rm := mapping()
		for _, kv := range [][2]string{{"http", r.http}, {"tcp", r.tcp}, {"log", r.log}, {"timeout", r.timeout}} {
			if kv[1] != "" {
				rm.Content = append(rm.Content, scalar(kv[0]), stringScalar(kv[1]))
			}
		}
		put(m, "ready", rm)
	}
	return m
}

func spaceServices(out []byte) []byte {
	lines := strings.Split(string(out), "\n")
	var b strings.Builder
	inServices := false
	for i, line := range lines {
		if strings.HasPrefix(line, "services:") {
			inServices = true
		}
		isServiceStart := inServices && i > 0 && len(line) > 2 && line[:2] == "  " && line[2] != ' ' && !strings.HasPrefix(lines[i-1], "  #") && !strings.HasPrefix(lines[i-1], "services:")
		isTopLevel := i > 0 && len(line) > 0 && line[0] != ' ' && line[0] != '#' && !strings.HasPrefix(lines[i-1], "#")
		if isServiceStart || isTopLevel && (strings.HasPrefix(line, "services:") || strings.HasPrefix(line, "env_files:")) {
			b.WriteString("\n")
		}
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

func mapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode} }

func put(m *yaml.Node, key string, val *yaml.Node) {
	m.Content = append(m.Content, scalar(key), val)
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

var yaml11Ambiguous = map[string]bool{"y": true, "n": true, "yes": true, "no": true, "on": true, "off": true, "true": true, "false": true, "null": true, "~": true}

func stringScalar(v string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	if yaml11Ambiguous[strings.ToLower(v)] {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

func scalarInt(n int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(n)}
}

func seq(items []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode}
	for _, it := range items {
		n.Content = append(n.Content, stringScalar(it))
	}
	return n
}

func flowSeq(items []string) *yaml.Node {
	n := seq(items)
	n.Style = yaml.FlowStyle
	return n
}

func shellSplit(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord, quote := false, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(s) && strings.IndexByte(`"\$`+"`", s[i+1]) >= 0:
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func shellJoin(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = shellQuote(w)
	}
	return strings.Join(out, " ")
}

func shellQuote(w string) string {
	if w == "" {
		return "''"
	}
	safe := true
	for _, c := range w {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_./:=@%+,", c)) {
			safe = false
			break
		}
	}
	if safe {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}
