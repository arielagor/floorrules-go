package memstore_test

import (
	"testing"

	"github.com/arielagor/floorrules/internal/store"
	"github.com/arielagor/floorrules/internal/store/memstore"
	"github.com/arielagor/floorrules/internal/store/storetest"
)

func TestContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return memstore.New() })
}
