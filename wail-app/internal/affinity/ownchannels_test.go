package affinity

import "testing"

func TestForeignChannelWithMatchingPeerNameIsNotOwn(t *testing.T) {
	// Regression: a third-party publisher (e.g. a DAW plugin) whose Link peer
	// name collides with the user's display name must stay capturable.
	o := NewOwnChannels()
	o.Published("GerenM · stream 0")
	if o.Own("aabbccdd", true, "Main") {
		t.Fatal("channel with colliding peer name but unminted name classified as own")
	}
}

func TestMintedNameWithMatchingPeerIsOwnAndLearnsID(t *testing.T) {
	o := NewOwnChannels()
	o.Published("GerenM · stream 0")
	if !o.Own("aabbccdd", true, "GerenM · stream 0") {
		t.Fatal("minted name + matching peer not classified as own")
	}
	// Once learned, the ID alone is decisive — even if the channel is renamed
	// to something never minted and the peer name no longer matches.
	if !o.Own("aabbccdd", false, "some totally different name") {
		t.Fatal("learned ID not classified as own after rename")
	}
}

func TestMintedNameWithoutPeerMatchIsNotOwn(t *testing.T) {
	// Another peer's channel that happens to share a minted name is not ours.
	o := NewOwnChannels()
	o.Published("GerenM · stream 0")
	if o.Own("aabbccdd", false, "GerenM · stream 0") {
		t.Fatal("foreign peer's channel classified as own by name alone")
	}
}

func TestRenamedSinkStaysOwnUnderOldAndNewName(t *testing.T) {
	// Affinity renames a sink in place when the real stream name arrives; both
	// names must classify as own (a stale discovery snapshot may carry the old).
	o := NewOwnChannels()
	o.Published("GerenM · stream 0")
	o.Published("GerenM · lead synth")
	if !o.Own("11223344", true, "GerenM · stream 0") {
		t.Fatal("old minted name no longer classified as own")
	}
	if !o.Own("55667788", true, "GerenM · lead synth") {
		t.Fatal("new minted name not classified as own")
	}
}

func TestUnknownChannelIsNotOwn(t *testing.T) {
	o := NewOwnChannels()
	if o.Own("aabbccdd", false, "Main") {
		t.Fatal("unknown channel classified as own")
	}
}
