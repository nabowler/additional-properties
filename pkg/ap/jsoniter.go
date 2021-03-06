package ap

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	jsoniter "github.com/json-iterator/go"
	"github.com/modern-go/reflect2"
	log "github.com/sirupsen/logrus"
)

type additionalPropertiesExtension struct {
	jsoniter.DummyExtension
	Desc      map[string]*jsoniter.StructDescriptor
	APBinding map[string]*jsoniter.Binding
	Mutex     *sync.Mutex
}

func newAdditionalPropertiesExtension() *additionalPropertiesExtension {
	return &additionalPropertiesExtension{
		DummyExtension: jsoniter.DummyExtension{},
		Desc:           map[string]*jsoniter.StructDescriptor{},
		APBinding:      map[string]*jsoniter.Binding{},
		Mutex:          &sync.Mutex{},
	}
}

// RegisterAdditionalPropertiesExtension registers the AP extension with
// the passed jsoniter.API
func RegisterAdditionalPropertiesExtension(api jsoniter.API) jsoniter.API {
	api.RegisterExtension(newAdditionalPropertiesExtension())
	return api
}

// ConfigCompatibleWithStandardLibrary provides a jsoniter API object
// that has already registered the additional-properties extension.
var ConfigCompatibleWithStandardLibrary = //nolint:gochecknoglobals
RegisterAdditionalPropertiesExtension(jsoniter.Config{
	EscapeHTML:             true,
	SortMapKeys:            true,
	ValidateJsonRawMessage: true,
}.Froze())

// UpdateStructDescriptor removes the wildcard field (if it exists) from
// the fields provided by the StructDescriptor and caches both the
// resulting field list and the AP field for decorator construction.
func (e *additionalPropertiesExtension) UpdateStructDescriptor(desc *jsoniter.StructDescriptor) {
	log.Debug("UpdateStructDescriptor")

	typ := typeName(desc.Type)
	log.Debug("Type: ", typ)

	e.Mutex.Lock()
	defer e.Mutex.Unlock()
	if _, ok := e.Desc[typ]; ok {
		log.Debug("Short-circuit: Descriptor already updated")
		return
	}

	e.Desc[typ] = desc

	log.Debug("Fields: ", desc.Fields)
	for idx, binding := range desc.Fields {
		if len(binding.FromNames) == 1 && binding.FromNames[0] == "*" {
			e.APBinding[typ] = binding
			desc.Fields = append(desc.Fields[:idx], desc.Fields[idx+1:]...)
			log.Debug("    AP binding: ", binding)
			break
		}
		log.Debug("    Field binding: ", binding)
	}
}

func (e *additionalPropertiesExtension) DecorateDecoder(
	typ reflect2.Type,
	decoder jsoniter.ValDecoder,
) jsoniter.ValDecoder {
	log.Trace("DecorateDecoder")
	name := typeName(typ)
	log.Debug("Type: ", name)

	if typ.Kind() != reflect.Struct {
		log.Debug("Not decorating encoder - not a struct: ", name)
		return decoder
	}

	e.Mutex.Lock()
	defer e.Mutex.Unlock()
	if e.APBinding[name] == nil && e.Desc[name] != nil {
		e.APBinding[name] = e.embeddedAPBinding(e.Desc[name].Type)
	}

	if e.APBinding[name] == nil {
		log.Debug("Not decorating encoder - no Additional Properties field")
		return decoder
	}

	log.Debug("Decorating decoder: ", name)
	fields := map[string]*jsoniter.Binding{}
	for _, binding := range e.Desc[name].Fields {
		fromName := binding.FromNames[0]
		fields[fromName] = binding
		fields[strings.ToLower(fromName)] = binding
	}

	return &apStructDecoder{fields, e.APBinding[name]}
}

type apStructDecoder struct {
	Fields    map[string]*jsoniter.Binding
	APBinding *jsoniter.Binding
}

func (d *apStructDecoder) Decode(ptr unsafe.Pointer, iter *jsoniter.Iterator) {
	log.Trace("apStructDecoder")
	ap := map[string]json.RawMessage{}
	if d.APBinding != nil {
		d.APBinding.Field.UnsafeSet(ptr, unsafe.Pointer(&ap))
	}

	for {
		key := iter.ReadObject()
		if key == "" {
			break
		}

		binding := d.Fields[key]
		if binding != nil {
			log.Debug("Case-sensitive binding: ", binding)
			binding.Decoder.Decode(ptr, iter)
			continue
		}

		// TODO: how do we gete the configuration value for case-sensitivity?
		binding = d.Fields[strings.ToLower(key)]
		if binding != nil {
			log.Debug("Case-insensitive binding: ", binding)
			binding.Decoder.Decode(ptr, iter)
			continue
		}

		var val json.RawMessage
		iter.ReadVal(&val)
		log.Debug("AP value: ", val)
		ap[key] = val
	}
}

func (e *additionalPropertiesExtension) DecorateEncoder(
	typ reflect2.Type,
	encoder jsoniter.ValEncoder,
) jsoniter.ValEncoder {
	log.Trace("DecorateEncoder")
	name := typeName(typ)
	log.Debug("Type: ", name)

	if typ.Kind() != reflect.Struct {
		log.Debug("Not decorating encoder - not a struct: ", name)
		return encoder
	}

	e.Mutex.Lock()
	defer e.Mutex.Unlock()
	if e.APBinding[name] == nil && e.Desc[name] != nil {
		e.APBinding[name] = e.embeddedAPBinding(e.Desc[name].Type)
	}

	apBinding, ok := e.APBinding[name]
	if !ok {
		log.Debug("Not decorating encoder - no AP field")
		return encoder
	}

	if apBinding == nil {
		log.Debug("Not decorating encoder - AP binding is nil")
		return encoder
	}

	log.Debug("Decorating encoder: ", name)
	fields := map[string]*jsoniter.Binding{}
	for _, binding := range e.Desc[name].Fields {
		toName := binding.ToNames[0]
		fields[toName] = binding
	}

	styp := typ.(reflect2.StructType)
	omitEmpties := omitEmpties(styp)
	return &apStructEncoder{fields, apBinding, omitEmpties}
}

func omitEmpties(typ reflect2.StructType) map[string]bool {
	empties := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Anonymous() {
			styp := f.Type().(reflect2.StructType)
			embeddedEmpties := omitEmpties(styp)
			for k := range embeddedEmpties {
				empties[k] = embeddedEmpties[k]
			}
			continue
		}
		name, qualifiers := jsonTag(f)
		log.Debug("Field name: ", name, ", qualifiers: ", qualifiers)
		empties[name] = qualifiers["omitempty"]
	}
	return empties
}

func jsonTag(f reflect2.StructField) (string, map[string]bool) {
	qualifiers := map[string]bool{}
	jt, ok := f.Tag().Lookup("json")
	if !ok {
		return f.Name(), qualifiers
	}
	parts := strings.Split(jt, ",")
	name := parts[0]
	if name == "" {
		name = f.Name()
	}
	for _, q := range parts[1:] {
		qualifiers[q] = true
	}
	return name, qualifiers
}

type apStructEncoder struct {
	Fields      map[string]*jsoniter.Binding
	APBinding   *jsoniter.Binding
	OmitEmpties map[string]bool
}

func (e *apStructEncoder) Encode(ptr unsafe.Pointer, stream *jsoniter.Stream) {
	log.Debug("apStructEncoder")

	log.Debugf("Stream: %v", ptr)
	stream.WriteObjectStart()
	log.Debug("Field count: ", len(e.Fields))

	first := true
	for key, binding := range e.Fields {
		log.Debug("Field key: ", key)
		if e.OmitEmpties[key] && binding.Encoder.IsEmpty(ptr) {
			log.Debug("Omitempty - key: ", key)
			continue
		}
		if !first {
			stream.WriteMore()
		}
		stream.WriteObjectField(key)
		binding.Encoder.Encode(ptr, stream)
		first = false
	}

	log.Debug("AP binding: ", e.APBinding)
	if e.APBinding == nil {
		stream.WriteObjectEnd()
		return
	}

	// Add the additional properties to the
	ap := *(*map[string]json.RawMessage)(e.APBinding.Field.UnsafeGet(ptr))
	log.Debug("AP: ", ap)
	for k, v := range ap {
		log.Debug("K: ", k, ", V: ", v)
		if !first {
			stream.WriteMore()
		}
		stream.WriteObjectField(k)
		stream.WriteVal(v)
		first = false
	}
	stream.WriteObjectEnd()
}

func (e *apStructEncoder) IsEmpty(ptr unsafe.Pointer) bool {
	return false
}

func (e *additionalPropertiesExtension) embeddedAPBinding(typ reflect2.Type) *jsoniter.Binding {
	str, ok := typ.(*reflect2.UnsafeStructType)
	if !ok {
		return nil
	}
	var ap *jsoniter.Binding
	for i := 0; i < str.NumField(); i++ {
		f := str.Field(i)
		if f.Anonymous() {
			name := typeName(f.Type())
			if a, ok := e.APBinding[name]; ok {
				ap = a
				break
			}
		}
	}
	return ap
}

func typeName(typ reflect2.Type) string {
	return fmt.Sprintf("%v", typ)
}
