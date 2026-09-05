package controller

import (
	"encoding/json"
	"errors"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
)

type Handler struct{ s *service.Service }

func New(s *service.Service) *Handler { return &Handler{s: s} }

func body(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "invalid JSON")
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "invalid JSON")
		return errors.New("request body contains multiple JSON values")
	}
	return nil
}

func out(w http.ResponseWriter, code int, v any) {
	if code == http.StatusNoContent {
		api.WriteNoContent(w)
		return
	}
	api.Success(w, code, v)
}

func fail(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, model.ErrNotFound):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, e.Error())
	case errors.Is(e, model.ErrInvalid):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, e.Error())
	default:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "internal server error")
	}
}

// API representations use stable, lower snake-case JSON names without changing the domain package.
type manufacturerJSON struct {
	ID             string `json:"id,omitempty"`
	Name           string `json:"name"`
	ContactName    string `json:"contact_name,omitempty"`
	ContactPhone   string `json:"contact_phone,omitempty"`
	Code           string `json:"code,omitempty"`
	MerchantID     string `json:"merchant_id,omitempty"`
	APIBaseURL     string `json:"api_base_url,omitempty"`
	TestAPIBaseURL string `json:"test_api_base_url,omitempty"`
	Status         string `json:"status,omitempty"`
}
type deviceJSON struct {
	ID               string          `json:"id,omitempty"`
	ManufacturerID   string          `json:"manufacturer_id"`
	Name             string          `json:"name"`
	SerialNumber     string          `json:"serial_number,omitempty"`
	Location         string          `json:"location,omitempty"`
	Status           string          `json:"status"`
	SerialUnique     string          `json:"serial_unique,omitempty"`
	DeviceName       string          `json:"device_name,omitempty"`
	ManufacturerCode string          `json:"manufacturer_code,omitempty"`
	StoreID          string          `json:"store_id,omitempty"`
	StoreName        string          `json:"store_name,omitempty"`
	Online           bool            `json:"online"`
	Version          string          `json:"version,omitempty"`
	Address          string          `json:"address,omitempty"`
	Error            string          `json:"error,omitempty"`
	LastActivityAt   time.Time       `json:"last_activity_at,omitempty"`
	DisplayConfig    json.RawMessage `json:"display_config,omitempty"`
	PaymentConfig    json.RawMessage `json:"payment_config,omitempty"`
}
type drinkJSON struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	Price           int64  `json:"price"`
	Status          string `json:"status"`
	OriginID        string `json:"origin_id,omitempty"`
	ProductNum      string `json:"product_num,omitempty"`
	EnName          string `json:"en_name,omitempty"`
	VIPPrice        int64  `json:"vip_price"`
	PickupCodePrice int64  `json:"pickup_code_price"`
	Image           string `json:"image,omitempty"`
	Sort            int    `json:"sort"`
}
type deviceDrinkJSON struct {
	DeviceID string `json:"device_id"`
	DrinkID  string `json:"drink_id,omitempty"`
	OriginID string `json:"origin_id,omitempty"`
	Enabled  bool   `json:"enabled"`
}

func manufacturerView(v model.Manufacturer) manufacturerJSON {
	return manufacturerJSON{ID: v.ID, Name: v.Name, ContactName: v.ContactName, ContactPhone: v.ContactPhone, Code: v.Code, MerchantID: v.MerchantID, APIBaseURL: v.APIBaseURL, TestAPIBaseURL: v.TestAPIBaseURL, Status: v.Status}
}
func deviceView(v model.Device) deviceJSON {
	return deviceJSON{ID: v.ID, ManufacturerID: v.ManufacturerID, Name: v.Name, SerialNumber: v.SerialNumber, Location: v.Location, Status: v.Status, SerialUnique: v.SerialUnique, DeviceName: v.DeviceName, ManufacturerCode: v.ManufacturerCode, StoreID: v.StoreID, StoreName: v.StoreName, Online: v.Online, Version: v.Version, Address: v.Address, Error: v.Error, LastActivityAt: v.LastActivityAt, DisplayConfig: v.DisplayConfig, PaymentConfig: v.PaymentConfig}
}
func drinkView(v model.Drink) drinkJSON {
	return drinkJSON{ID: v.ID, Name: v.Name, Description: v.Description, Price: v.Price, Status: v.Status, OriginID: v.OriginID, ProductNum: v.ProductNum, EnName: v.EnName, VIPPrice: v.VIPPrice, PickupCodePrice: v.PickupCodePrice, Image: v.Image, Sort: v.Sort}
}
func relationView(v model.DeviceDrink) deviceDrinkJSON {
	return deviceDrinkJSON{DeviceID: v.DeviceID, DrinkID: v.DrinkID, OriginID: v.OriginID, Enabled: v.Enabled}
}

func listFilter(q map[string][]string, status, manufacturer, name string) bool {
	if v := first(q, "status"); v != "" && !strings.EqualFold(v, status) {
		return false
	}
	if v := first(q, "manufacturer_id"); v != "" && v != manufacturer {
		return false
	}
	if v := first(q, "q"); v != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(v)) {
		return false
	}
	return true
}

func deviceListFilter(q map[string][]string, v model.Device) bool {
	if !listFilter(q, v.Status, v.ManufacturerID, v.Name) {
		return false
	}
	if value := first(q, "serial_unique"); value != "" && value != v.SerialUnique {
		return false
	}
	if value := first(q, "store_id"); value != "" && value != v.StoreID {
		return false
	}
	if value := first(q, "online"); value != "" {
		online, err := strconv.ParseBool(value)
		if err != nil || online != v.Online {
			return false
		}
	}
	return true
}

func drinkListFilter(q map[string][]string, v model.Drink) bool {
	if !listFilter(q, v.Status, "", v.Name) {
		return false
	}
	if value := first(q, "product_num"); value != "" && value != v.ProductNum {
		return false
	}
	if value := first(q, "origin_id"); value != "" && value != v.OriginID {
		return false
	}
	return true
}
func first(q map[string][]string, key string) string {
	if v := q[key]; len(v) > 0 {
		return strings.TrimSpace(v[0])
	}
	return ""
}

func (h *Handler) Collection(w http.ResponseWriter, r *http.Request, kind string) {
	prefix := "/v1/coffee-machine/" + kind
	path := strings.TrimSuffix(r.URL.Path, "/")
	rest := strings.TrimPrefix(path, prefix)
	if rest != "" && (rest[0] != '/' || strings.Count(rest, "/") != 1 || strings.TrimPrefix(rest, "/") == "") {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "not found")
		return
	}
	id := strings.TrimPrefix(rest, "/")
	if r.Method == http.MethodGet {
		if id != "" {
			h.get(w, r, kind, id)
			return
		}
		switch kind {
		case "manufacturers":
			v, e := h.s.ListManufacturers(r.Context())
			if e != nil {
				fail(w, e)
				return
			}
			result := make([]manufacturerJSON, 0, len(v))
			for _, x := range v {
				result = append(result, manufacturerView(x))
			}
			out(w, 200, result)
		case "devices":
			v, e := h.s.ListDevices(r.Context())
			if e != nil {
				fail(w, e)
				return
			}
			result := make([]deviceJSON, 0, len(v))
			for _, x := range v {
				if deviceListFilter(r.URL.Query(), x) {
					result = append(result, deviceView(x))
				}
			}
			out(w, 200, result)
		case "drinks":
			v, e := h.s.ListDrinks(r.Context())
			if e != nil {
				fail(w, e)
				return
			}
			result := make([]drinkJSON, 0, len(v))
			for _, x := range v {
				if drinkListFilter(r.URL.Query(), x) {
					result = append(result, drinkView(x))
				}
			}
			out(w, 200, result)
		}
		return
	}
	if r.Method == http.MethodPost && id == "" && kind == "manufacturers" {
		h.create(w, r, kind)
		return
	}
	if id != "" && (r.Method == http.MethodPut || r.Method == http.MethodPatch) {
		h.update(w, r, kind, id)
		return
	}
	if id != "" && r.Method == http.MethodDelete && kind == "manufacturers" {
		h.delete(w, r, kind, id)
		return
	}
	w.Header().Set("Allow", allow(kind, id))
	api.Error(w, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed, "method not allowed")
}

func allow(kind, id string) string {
	if id == "" {
		if kind == "manufacturers" {
			return "GET, POST"
		}
		return "GET"
	}
	if kind == "devices" || kind == "drinks" {
		return "GET, PUT, PATCH"
	}
	return "GET, PUT, PATCH, DELETE"
}
func (h *Handler) get(w http.ResponseWriter, r *http.Request, k, id string) {
	switch k {
	case "manufacturers":
		v, e := h.s.GetManufacturer(r.Context(), id)
		if e != nil {
			fail(w, e)
		} else {
			out(w, 200, manufacturerView(v))
		}
	case "devices":
		v, e := h.s.GetDevice(r.Context(), id)
		if e != nil {
			fail(w, e)
		} else {
			out(w, 200, deviceView(v))
		}
	case "drinks":
		v, e := h.s.GetDrink(r.Context(), id)
		if e != nil {
			fail(w, e)
		} else {
			out(w, 200, drinkView(v))
		}
	}
}
func (h *Handler) create(w http.ResponseWriter, r *http.Request, k string) {
	switch k {
	case "manufacturers":
		var v manufacturerJSON
		if body(w, r, &v) != nil {
			return
		}
		x, e := h.s.CreateManufacturer(r.Context(), model.Manufacturer{ID: v.ID, Name: v.Name, ContactName: v.ContactName, ContactPhone: v.ContactPhone, Code: v.Code, MerchantID: v.MerchantID, APIBaseURL: v.APIBaseURL, TestAPIBaseURL: v.TestAPIBaseURL, Status: v.Status})
		if e != nil {
			fail(w, e)
		} else {
			out(w, 201, manufacturerView(x))
		}
	}
}
func (h *Handler) update(w http.ResponseWriter, r *http.Request, k, id string) {
	switch k {
	case "manufacturers":
		var v manufacturerJSON
		if body(w, r, &v) != nil {
			return
		}
		x, e := h.s.UpdateManufacturer(r.Context(), id, model.Manufacturer{Name: v.Name, ContactName: v.ContactName, ContactPhone: v.ContactPhone, Code: v.Code, MerchantID: v.MerchantID, APIBaseURL: v.APIBaseURL, TestAPIBaseURL: v.TestAPIBaseURL, Status: v.Status})
		if e != nil {
			fail(w, e)
		} else {
			out(w, 200, manufacturerView(x))
		}
	case "devices":
		var v deviceJSON
		if body(w, r, &v) != nil {
			return
		}
		x, e := h.s.UpdateDevice(r.Context(), id, model.Device{ManufacturerID: v.ManufacturerID, Name: v.Name, SerialNumber: v.SerialNumber, Location: v.Location, Status: v.Status, SerialUnique: v.SerialUnique, DeviceName: v.DeviceName, ManufacturerCode: v.ManufacturerCode, StoreID: v.StoreID, StoreName: v.StoreName, Online: v.Online, Version: v.Version, Address: v.Address, Error: v.Error, LastActivityAt: v.LastActivityAt, DisplayConfig: v.DisplayConfig, PaymentConfig: v.PaymentConfig})
		if e != nil {
			fail(w, e)
		} else {
			out(w, 200, deviceView(x))
		}
	case "drinks":
		var v drinkJSON
		if body(w, r, &v) != nil {
			return
		}
		x, e := h.s.UpdateDrink(r.Context(), id, model.Drink{Name: v.Name, Description: v.Description, Price: v.Price, Status: v.Status, OriginID: v.OriginID, ProductNum: v.ProductNum, EnName: v.EnName, VIPPrice: v.VIPPrice, PickupCodePrice: v.PickupCodePrice, Image: v.Image, Sort: v.Sort})
		if e != nil {
			fail(w, e)
		} else {
			out(w, 200, drinkView(x))
		}
	}
}
func (h *Handler) delete(w http.ResponseWriter, r *http.Request, k, id string) {
	var e error
	switch k {
	case "manufacturers":
		e = h.s.DeleteManufacturer(r.Context(), id)
	case "devices":
		e = h.s.DeleteDevice(r.Context(), id)
	case "drinks":
		e = h.s.DeleteDrink(r.Context(), id)
	}
	if e != nil {
		fail(w, e)
	} else {
		out(w, http.StatusNoContent, nil)
	}
}

func (h *Handler) Relations(w http.ResponseWriter, r *http.Request) {
	prefix := "/v1/coffee-machine/devices/"
	rest := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), prefix)
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] != "drinks" || (len(parts) == 3 && parts[2] == "") || len(parts) > 3 {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "not found")
		return
	}
	d := parts[0]
	if r.Method == http.MethodGet && len(parts) == 2 {
		v, e := h.s.ListDeviceDrinks(r.Context(), d)
		if e != nil {
			fail(w, e)
		} else {
			a := make([]deviceDrinkJSON, 0, len(v))
			for _, x := range v {
				a = append(a, relationView(x))
			}
			out(w, 200, a)
		}
		return
	}
	w.Header().Set("Allow", "GET")
	api.Error(w, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed, "method not allowed")
}
