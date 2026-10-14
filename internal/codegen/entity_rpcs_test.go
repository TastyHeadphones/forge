package codegen

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// EntityRPCs lists exactly the declared rpcs that claim an entity, with the
// proto line each is declared on — reliant's messages table, claimed by
// MessageService.GetMessage and ChatService.ListMessages, the latter on a
// service whose Get/Create/… the CRUD quintet would never name.
func TestEntityRPCs_DeclaredUnmarkedWithLocations(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("proto/app/v1/message.proto", `syntax = "proto3";
package app.v1;

service MessageService {
  // GetMessage reads one.
  rpc GetMessage(GetMessageRequest) returns (GetMessageResponse) {}
}
`)
	write("proto/app/v1/chat.proto", `syntax = "proto3";
package app.v1;

service OtherService {
  rpc ListMessages(A) returns (B) {}
}

service ChatService {
  rpc SendMessage(A) returns (B) {}
  rpc ListMessages(ListMessagesRequest) returns (ListMessagesResponse) {}
  // forge:custom
  rpc DeleteMessage(A) returns (B) {}
}
`)
	msgs := map[string][]MessageFieldDef{"Message": nil}
	services := []ServiceDef{
		{Name: "MessageService", Package: "app.v1", ProtoFile: "app/v1/message.proto", Messages: msgs,
			Methods: []Method{{Name: "GetMessage"}}},
		{Name: "ChatService", Package: "app.v1", ProtoFile: "app/v1/chat.proto", Messages: msgs,
			Methods: []Method{{Name: "SendMessage"}, {Name: "ListMessages"}, {Name: "DeleteMessage", Custom: true}}},
		// Names Message in an rpc but declares no Message wire type: claims nothing.
		{Name: "OtherService", Package: "app.v1", ProtoFile: "app/v1/chat.proto",
			Messages: map[string][]MessageFieldDef{"A": nil}, Methods: []Method{{Name: "ListMessages"}}},
	}
	got := EntityRPCs(dir, services, []string{"Message"})
	want := []string{
		"MessageService.GetMessage (proto/app/v1/message.proto:6)",
		"ChatService.ListMessages (proto/app/v1/chat.proto:10)",
	}
	var rendered []string
	for _, r := range got["Message"] {
		rendered = append(rendered, r.String())
	}
	if !reflect.DeepEqual(rendered, want) {
		t.Errorf("EntityRPCs = %v\nwant          %v", rendered, want)
	}
}
