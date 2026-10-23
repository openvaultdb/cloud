package publisherselection

import "testing"

func TestStrictPublisherSelection(t *testing.T) {
	valid := "format: ovdb-manifest/draft-1\nid: fixture\nrecordsets: [one, two]\n"
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		valid + "unknown: true\n", valid + "recordsets: [one]\n",
		"format: ovdb-manifest/draft-1\nrecordsets: [one, one]\n",
		"format: ovdb-manifest/draft-1\nrecordsets: [1]\n",
		"format: ovdb-manifest/draft-1\nrecordsets: &names [one]\n",
		"format: ovdb-manifest/draft-1\nrecordsets: null\n",
		valid + "---\nrecordsets: [three]\n",
	} {
		if _, err := Parse([]byte(text)); err == nil {
			t.Fatalf("accepted invalid YAML %q", text)
		}
	}
}
