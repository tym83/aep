package changeapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestStorageErrorMapping(t *testing.T) {
	gr := schema.GroupResource{Group: "aep.aenix.io", Resource: "changerequests"}
	for _, tc := range []struct {
		err  error
		code int
		hint string
	}{
		{apierrors.NewForbidden(gr, "chg-1", errors.New("rbac")), http.StatusServiceUnavailable, "not configured for this organization"},
		{apierrors.NewInvalid(schema.GroupKind{Group: "aep.aenix.io", Kind: "ChangeRequest"}, "chg-1", field.ErrorList{field.TooLong(field.NewPath("spec", "target", "name"), "x", 63)}), http.StatusBadRequest, "spec.target.name"},
		{errors.New("boom"), http.StatusInternalServerError, "cannot persist the change"},
	} {
		w := httptest.NewRecorder()
		storageError(w, "cannot persist the change", tc.err)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.hint) {
			t.Fatalf("%v: got %d %s", tc.err, w.Code, w.Body.String())
		}
		if tc.code == http.StatusInternalServerError && strings.Contains(w.Body.String(), "boom") {
			t.Fatal("internal error details leaked")
		}
	}
}
