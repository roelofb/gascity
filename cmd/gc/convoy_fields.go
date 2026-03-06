package main

import (
	"github.com/gastownhall/gascity/internal/beads"
)

// ConvoyFields holds structured metadata for convoy beads. These map to
// individual key-value pairs stored via Store.SetMetadata.
type ConvoyFields struct {
	Owner    string // who manages this convoy
	Notify   string // notification target on completion
	Molecule string // associated molecule ID
	Merge    string // merge strategy: "direct", "mr", "local"
}

// convoyFieldKeys maps ConvoyFields struct fields to their metadata key names.
var convoyFieldKeys = [...]struct {
	key    string
	getter func(*ConvoyFields) string
	setter func(*ConvoyFields, string)
}{
	{"convoy.owner", func(f *ConvoyFields) string { return f.Owner }, func(f *ConvoyFields, v string) { f.Owner = v }},
	{"convoy.notify", func(f *ConvoyFields) string { return f.Notify }, func(f *ConvoyFields, v string) { f.Notify = v }},
	{"convoy.molecule", func(f *ConvoyFields) string { return f.Molecule }, func(f *ConvoyFields, v string) { f.Molecule = v }},
	{"convoy.merge", func(f *ConvoyFields) string { return f.Merge }, func(f *ConvoyFields, v string) { f.Merge = v }},
}

// applyConvoyFields populates a Bead's Metadata map with non-empty ConvoyFields.
// Call before store.Create to include metadata atomically in the creation.
func applyConvoyFields(b *beads.Bead, fields ConvoyFields) {
	for _, kv := range convoyFieldKeys {
		v := kv.getter(&fields)
		if v == "" {
			continue
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]string)
		}
		b.Metadata[kv.key] = v
	}
}

// setConvoyFields writes non-empty ConvoyFields to the bead store as metadata.
// Used for post-creation updates (e.g., adding fields to an existing convoy).
func setConvoyFields(store beads.Store, id string, fields ConvoyFields) error {
	for _, kv := range convoyFieldKeys {
		v := kv.getter(&fields)
		if v == "" {
			continue
		}
		if err := store.SetMetadata(id, kv.key, v); err != nil {
			return err
		}
	}
	return nil
}

// getConvoyFields reads ConvoyFields from a bead's Metadata map.
// Returns empty fields for keys that are not set.
func getConvoyFields(b beads.Bead) ConvoyFields {
	var fields ConvoyFields
	if b.Metadata == nil {
		return fields
	}
	for _, kv := range convoyFieldKeys {
		if v, ok := b.Metadata[kv.key]; ok {
			kv.setter(&fields, v)
		}
	}
	return fields
}
