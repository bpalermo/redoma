package relayv1_test

import (
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// syntaxPath is the source path of the file's syntax or edition statement
// (FileDescriptorProto field 12), whose leading comment is the file's own.
var syntaxPath = protoreflect.SourcePath{12}

// undocumented reports the elements of fd with no leading comment, by full
// name: the comments are what the protocol's documentation is generated
// from, so the file itself and every message, field, oneof, enum, enum
// value, service and rpc need one.
func undocumented(fd protoreflect.FileDescriptor) []string {
	var missing []string
	if strings.TrimSpace(fd.SourceLocations().ByPath(syntaxPath).LeadingComments) == "" {
		missing = append(missing, fd.Path()+" (the file comment, before the edition statement)")
	}
	check := func(d protoreflect.Descriptor) {
		if strings.TrimSpace(fd.SourceLocations().ByDescriptor(d).LeadingComments) == "" {
			missing = append(missing, string(d.FullName()))
		}
	}
	var messages func(protoreflect.MessageDescriptors)
	enums := func(es protoreflect.EnumDescriptors) {
		for i := range es.Len() {
			e := es.Get(i)
			check(e)
			for j := range e.Values().Len() {
				check(e.Values().Get(j))
			}
		}
	}
	messages = func(ms protoreflect.MessageDescriptors) {
		for i := range ms.Len() {
			m := ms.Get(i)
			check(m)
			for j := range m.Fields().Len() {
				check(m.Fields().Get(j))
			}
			for j := range m.Oneofs().Len() {
				check(m.Oneofs().Get(j))
			}
			enums(m.Enums())
			messages(m.Messages())
		}
	}
	messages(fd.Messages())
	enums(fd.Enums())
	for i := range fd.Services().Len() {
		s := fd.Services().Get(i)
		check(s)
		for j := range s.Methods().Len() {
			check(s.Methods().Get(j))
		}
	}
	return missing
}

// TestEveryElementDocumented: relay.proto, read with its comments
// (relay_source_info.binpb, protoc --include_source_info), has a leading
// comment on every element.
func TestEveryElementDocumented(t *testing.T) {
	raw, err := os.ReadFile("relay_source_info.binpb")
	if err != nil {
		t.Fatal(err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := files.FindFileByPath("redoma/relay/v1/relay.proto")
	if err != nil {
		t.Fatal(err)
	}
	if fd.Messages().Len() == 0 || fd.Services().Len() == 0 {
		t.Fatal("relay.proto has no messages or services: not the file under test")
	}
	if missing := undocumented(fd); len(missing) > 0 {
		t.Errorf("undocumented in relay.proto: %s", strings.Join(missing, ", "))
	}
}
