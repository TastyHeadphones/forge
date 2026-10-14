package codegen

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jinzhu/inflection"
)

// EntityRPC is one declared rpc that makes a table an entity: unary, not
// `// forge:custom`, named Create<E>/Get<E>/Update<E>/Delete<E>/List<Es> on a
// service that declares the wire message <E>. At names it in the proto
// source ("proto/reliant/v1/message.proto:14"), or is "" when the line could
// not be found.
type EntityRPC struct {
	Service string
	Method  string
	At      string
}

func (r EntityRPC) String() string {
	if r.At == "" {
		return r.Service + "." + r.Method
	}
	return r.Service + "." + r.Method + " (" + r.At + ")"
}

// EntityRPCs returns, for each entity name asked about, the declared rpcs
// that claim it — by the same rule entitiesForTables applies, so they are
// exactly the rpcs whose `// forge:custom` would make the table plain schema.
// It is what the advice for keeping a table off forge's ORM must name: the
// rpcs the project HAS, not the five a CRUD quintet would have.
func EntityRPCs(projectDir string, services []ServiceDef, entities []string) map[string][]EntityRPC {
	want := map[string]string{} // lower-case → entity as asked
	for _, e := range entities {
		want[strings.ToLower(e)] = e
	}
	out := map[string][]EntityRPC{}
	for _, svc := range services {
		for _, m := range svc.Methods {
			op, name := CRUDShape(m)
			if op == "" {
				continue
			}
			if op == "list" {
				name = inflection.Singular(name)
			}
			entity, ok := want[strings.ToLower(name)]
			if !ok || !declaresWireMessage(svc, name) {
				continue
			}
			out[entity] = append(out[entity], EntityRPC{
				Service: svc.Name, Method: m.Name, At: rpcLocation(projectDir, svc, m.Name),
			})
		}
	}
	return out
}

// rpcLocation finds `rpc <name>(` inside `service <svc.Name> {` of the proto
// file declaring svc.
func rpcLocation(projectDir string, svc ServiceDef, rpc string) string {
	if svc.ProtoFile == "" {
		return ""
	}
	rel := filepath.ToSlash(filepath.Join("proto", filepath.FromSlash(svc.ProtoFile)))
	f, err := os.Open(filepath.Join(projectDir, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	serviceRe := regexp.MustCompile(`^\s*service\s+` + regexp.QuoteMeta(svc.Name) + `\s*\{`)
	rpcRe := regexp.MustCompile(`^\s*rpc\s+` + regexp.QuoteMeta(rpc) + `\s*\(`)
	inService := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		switch {
		case serviceRe.MatchString(line):
			inService = true
		case inService && rpcRe.MatchString(line):
			return fmt.Sprintf("%s:%d", rel, n)
		case inService && strings.HasPrefix(strings.TrimSpace(line), "service "):
			inService = false
		}
	}
	return ""
}
