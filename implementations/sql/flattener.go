package sql

import (
	"fmt"
	jsoniter "github.com/json-iterator/go"
	"reflect"
	"strings"
)

var DefaultFlattener = NewFlattener()

type Flattener interface {
	FlattenObject(map[string]any) (map[string]any, error)
}

type FlattenerImpl struct {
	omitNilValues bool
}

func NewFlattener() Flattener {
	return &FlattenerImpl{
		omitNilValues: true,
	}
}

// FlattenObject flatten object e.g. from {"key1":{"key2":123}} to {"key1_key2":123}
// from {"$key1":1} to {"_key1":1}
// from {"(key1)":1} to {"_key1_":1}
func (f *FlattenerImpl) FlattenObject(json map[string]any) (map[string]any, error) {
	flattenMap := make(map[string]any)

	err := f.flatten("", json, flattenMap)
	if err != nil {
		return nil, err
	}
	emptyKeyValue, hasEmptyKey := flattenMap[""]
	if hasEmptyKey {
		flattenMap["_unnamed"] = emptyKeyValue
		delete(flattenMap, "")
	}
	return flattenMap, nil
}

// recursive function for flatten key (if value is inner object -> recursion call)
// Reformat key
func (f *FlattenerImpl) flatten(key string, value any, destination map[string]any) error {
	t := reflect.ValueOf(value)
	switch t.Kind() {
	case reflect.Slice:
		if strings.Contains(key, SqlTypeKeyword) {
			//meta field. value must be left untouched.
			destination[key] = value
			return nil
		}
		b, err := jsoniter.Marshal(value)
		if err != nil {
			return fmt.Errorf("Error marshaling array with key %s: %v", key, err)
		}
		destination[key] = string(b)
	case reflect.Map:
		unboxed := value.(map[string]any)
		for k, v := range unboxed {
			newKey := k
			if key != "" {
				newKey = key + "_" + newKey
			}
			if err := f.flatten(newKey, v, destination); err != nil {
				return err
			}
		}
	case reflect.Bool:
		boolValue, _ := value.(bool)
		destination[key] = boolValue
	default:
		if !f.omitNilValues || value != nil {
			switch value.(type) {
			case string:
				strValue, _ := value.(string)

				destination[key] = strValue
			default:
				destination[key] = value
			}
		}
	}

	return nil
}

type DummyFlattener struct {
}

func NewDummyFlattener() *DummyFlattener {
	return &DummyFlattener{}
}

// FlattenObject return the same json object
func (df *DummyFlattener) FlattenObject(json map[string]any) (map[string]any, error) {
	return json, nil
}
