package validation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/openshift-online/rosa-seaplane-api/featuregate"
	"github.com/openshift-online/rosa-seaplane-api/hack/api-codegen/pkg/registry"
)

// PreserveServiceSet copies stored service-set values into a merged update
// candidate. Validation rejects changing or clearing a service-set field, but an
// explicit null on a parent (e.g. configuration.proxy) is a structural container
// it skips, and the merge would clear the platform-managed children with it.
// Structural containers are skipped here too so their mutable descendants stay editable.
func (v *FieldValidator) PreserveServiceSet(candidate, stored any, fs featuregate.FeatureSet) error {
	want, err := specMap(stored)
	if err != nil {
		return err
	}
	got, err := specMap(candidate)
	if err != nil {
		return err
	}

	fieldMetaMap := v.typedRegistry[v.resourceType]
	isChanged := false
	for path, meta := range fieldMetaMap {
		if effectiveWriteMode(meta, fs) != registry.ServiceSet || isStructuralContainer(path, fieldMetaMap) {
			continue
		}
		keys := strings.Split(strings.TrimPrefix(path, "spec."), ".")
		storedValue, hasStored := lookupPath(want, keys)
		currentValue, hasCurrent := lookupPath(got, keys)
		if hasStored == hasCurrent && reflect.DeepEqual(storedValue, currentValue) {
			continue
		}
		if err := setPath(got, keys, storedValue, hasStored); err != nil {
			return fmt.Errorf("preserving %s: %w", path, err)
		}
		isChanged = true
	}
	if !isChanged {
		return nil
	}

	// Rebuild the candidate from the corrected map; spec types round-trip through JSON.
	data, err := json.Marshal(got)
	if err != nil {
		return err
	}
	target := reflect.ValueOf(candidate).Elem()
	target.Set(reflect.Zero(target.Type()))
	return json.Unmarshal(data, candidate)
}

func specMap(spec any) (map[string]any, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func lookupPath(m map[string]any, keys []string) (any, bool) {
	var current any = m
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = object[key]; !ok {
			return nil, false
		}
	}
	return current, true
}

// setPath writes or deletes a value, creating intermediate objects as needed.
func setPath(m map[string]any, keys []string, value any, isPresent bool) error {
	object := m
	for _, key := range keys[:len(keys)-1] {
		next, exists := object[key]
		if !exists || next == nil {
			if !isPresent {
				return nil
			}
			next = map[string]any{}
			object[key] = next
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not an object", key)
		}
		object = child
	}
	last := keys[len(keys)-1]
	if !isPresent {
		delete(object, last)
		return nil
	}
	object[last] = value
	return nil
}
