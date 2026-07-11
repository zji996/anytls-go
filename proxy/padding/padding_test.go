package padding

import (
	"bytes"
	"testing"
)

func TestNewDefaultPaddingFactoryIsIndependent(t *testing.T) {
	clientPadding := NewDefaultPaddingFactory()
	original := DefaultPaddingFactory.Load()

	customScheme := []byte("stop=1\n0=12-12")
	if !UpdatePaddingFactory(clientPadding, customScheme) {
		t.Fatal("UpdatePaddingFactory failed")
	}

	if clientPadding.Load().Md5 == original.Md5 {
		t.Fatal("client padding did not change")
	}
	if DefaultPaddingFactory.Load().Md5 != original.Md5 {
		t.Fatal("default padding factory was modified by client-scoped update")
	}
}

func TestUpdatePaddingSchemeUpdatesDefaultFactory(t *testing.T) {
	original := DefaultPaddingFactory.Load()
	defer DefaultPaddingFactory.Store(original)

	customScheme := []byte("stop=1\n0=12-12")
	if !UpdatePaddingScheme(customScheme) {
		t.Fatal("UpdatePaddingScheme failed")
	}
	if DefaultPaddingFactory.Load().Md5 == original.Md5 {
		t.Fatal("default padding factory did not change")
	}
}

func TestNewPaddingFactoryRejectsInvalidBounds(t *testing.T) {
	tests := []string{
		"stop=-1",
		"stop=1\n0=1-65536",
		"stop=1\n0=invalid",
		"stop=1\ninvalid=1-2",
		"stop=1\n0=c",
		"stop=1\n0=1-2,3-4",
	}
	for _, scheme := range tests {
		if factory := NewPaddingFactory([]byte(scheme)); factory != nil {
			t.Fatalf("NewPaddingFactory(%q) unexpectedly succeeded", scheme)
		}
	}
}

func TestNewPaddingFactoryOwnsRawScheme(t *testing.T) {
	raw := []byte("stop=1\n0=12-12")
	factory := NewPaddingFactory(raw)
	if factory == nil {
		t.Fatal("NewPaddingFactory failed")
	}
	want := append([]byte(nil), raw...)
	raw[0] = 'x'
	if !bytes.Equal(factory.RawScheme, want) {
		t.Fatalf("RawScheme changed with caller buffer: %q", factory.RawScheme)
	}
}
