package padding

import "testing"

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
