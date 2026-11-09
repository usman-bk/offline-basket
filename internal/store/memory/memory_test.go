package memory_test

import (
	"testing"

	"github.com/usmanvirk/offline-basket/internal/basket"
	"github.com/usmanvirk/offline-basket/internal/store/memory"
	"github.com/usmanvirk/offline-basket/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) basket.Store { return memory.New() })
}
