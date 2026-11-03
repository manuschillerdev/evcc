package charger

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util/sponsor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const goELocalFixture = `{"car":2,"err":0,"alw":true,"eto":1234567,"nrg":[230.1,231.2,232.3,0,6.1,6.2,6.3,1403.61,1433.44,1463.49,0,4300.54,1,1,1,1],"var":11,"mca":6,"ama":16,"psm":0}`

func goELocalServer(t *testing.T, status string, write http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		switch r.URL.Path {
		case "/api/status":
			assert.ElementsMatch(t, []string{"car", "err", "alw", "eto", "nrg", "var", "mca", "ama", "psm"}, strings.Split(r.URL.Query().Get("filter"), ","))
			fmt.Fprint(w, status)
		case "/api/set":
			if write != nil {
				write(w, r)
				return
			}
			t.Error("unexpected write")
			w.WriteHeader(http.StatusBadRequest)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func goELocalCharger(t *testing.T, srv *httptest.Server) *GoELocal {
	t.Helper()
	c, err := NewGoELocalFromConfig(t.Context(), map[string]any{"uri": srv.URL, "cache": "0s"})
	require.NoError(t, err)
	return c.(*GoELocal)
}

func TestGoELocalTemplate(t *testing.T) {
	previousSubject := sponsor.Subject
	sponsor.Subject = ""
	t.Cleanup(func() { sponsor.Subject = previousSubject })

	srv := goELocalServer(t, goELocalFixture, nil)
	c, err := NewFromConfig(t.Context(), "template", map[string]any{
		"template": "go-e-local",
		"host":     strings.TrimPrefix(srv.URL, "http://"),
	})
	require.NoError(t, err)
	assert.IsType(t, &GoELocal{}, c)
	assert.True(t, api.HasCap[api.PhaseSwitcher](c))
	assert.True(t, api.HasCap[api.Meter](c))
	assert.True(t, api.HasCap[api.MeterEnergy](c))
	assert.True(t, api.HasCap[api.CurrentLimiter](c))
	assert.False(t, api.HasCap[api.ChargeRater](c))
}

func TestGoELocalStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		car    int
		fault  int
		status api.ChargeStatus
		err    string
	}{
		{"disconnected", 1, 0, api.StatusA, ""},
		{"charging", 2, 0, api.StatusC, ""},
		{"waiting", 3, 0, api.StatusB, ""},
		{"complete", 4, 0, api.StatusB, ""},
		{"unknown", 0, 0, api.StatusNone, "invalid car state"},
		{"error", 5, 0, api.StatusNone, "invalid car state"},
		{"initializing", 6, 0, api.StatusNone, "invalid car state"},
		{"unexpected", 99, 0, api.StatusNone, "invalid car state"},
		{"fault while connected", 3, 13, api.StatusNone, "charger error: 13"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"car":%d,"err":%d,"alw":false,"psm":1}`, tc.car, tc.fault)
			c := goELocalCharger(t, goELocalServer(t, body, nil))
			status, err := c.Status()
			assert.Equal(t, tc.status, status)
			if tc.err == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tc.err)
			}
			enabled, err := c.Enabled()
			assert.NoError(t, err)
			assert.False(t, enabled)
		})
	}
}

func TestGoELocalMetering(t *testing.T) {
	c := goELocalCharger(t, goELocalServer(t, goELocalFixture, nil))
	power, err := c.CurrentPower()
	require.NoError(t, err)
	assert.InDelta(t, 4300.54, power, 0.001)
	energy, err := c.TotalEnergy()
	require.NoError(t, err)
	assert.InDelta(t, 1234.567, energy, 0.001)
	i1, i2, i3, err := c.Currents()
	require.NoError(t, err)
	assert.Equal(t, []float64{6.1, 6.2, 6.3}, []float64{i1, i2, i3})
	u1, u2, u3, err := c.Voltages()
	require.NoError(t, err)
	assert.Equal(t, []float64{230.1, 231.2, 232.3}, []float64{u1, u2, u3})
	enabled, err := c.Enabled()
	require.NoError(t, err)
	assert.True(t, enabled)
}

func TestGoELocalCommands(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
		call  func(*GoELocal) error
	}{
		{"enable", "frc", "2", func(c *GoELocal) error { return c.Enable(true) }},
		{"disable", "frc", "1", func(c *GoELocal) error { return c.Enable(false) }},
		{"minimum current", "amp", "6", func(c *GoELocal) error { return c.MaxCurrent(6) }},
		{"maximum current", "amp", "16", func(c *GoELocal) error { return c.MaxCurrent(16) }},
		{"one phase", "psm", "1", func(c *GoELocal) error { return c.Phases1p3p(1) }},
		{"three phases", "psm", "2", func(c *GoELocal) error { return c.Phases1p3p(3) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			srv := goELocalServer(t, goELocalFixture, func(w http.ResponseWriter, r *http.Request) {
				writes.Add(1)
				assert.Equal(t, url.Values{tc.key: {tc.value}}, r.URL.Query())
				fmt.Fprintf(w, `{"%s":true}`, tc.key)
			})
			c := goELocalCharger(t, srv)
			require.NoError(t, tc.call(c))
			assert.EqualValues(t, 1, writes.Load())
		})
	}
}

func TestGoELocalRejectsInvalidControls(t *testing.T) {
	c := goELocalCharger(t, goELocalServer(t, goELocalFixture, nil))
	for _, current := range []int64{-1, 0, 5, 17, 32, math.MaxInt64} {
		assert.Error(t, c.MaxCurrent(current))
	}
	for _, phases := range []int{-1, 0, 2, 4} {
		assert.Error(t, c.Phases1p3p(phases))
	}
}

func TestGoELocalCurrentLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		variant int
		minimum int
		maximum int
		wantMin float64
		wantMax float64
		wantErr bool
	}{
		{"11 kW hardware", 11, 6, 32, 6, 16, false},
		{"22 kW hardware", 22, 6, 32, 6, 32, false},
		{"user maximum", 22, 6, 10, 6, 10, false},
		{"user minimum", 22, 8, 16, 8, 16, false},
		{"minimum floor", 11, 0, 16, 6, 16, false},
		{"unknown variant", 0, 6, 16, 0, 0, true},
		{"inconsistent limits", 11, 20, 16, 0, 0, true},
		{"invalid maximum", 22, 6, 0, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"car":1,"err":0,"alw":false,"psm":0,"var":%d,"mca":%d,"ama":%d}`, tc.variant, tc.minimum, tc.maximum)
			c := goELocalCharger(t, goELocalServer(t, body, nil))
			lower, upper, err := c.GetMinMaxCurrent()
			if tc.wantErr {
				assert.Error(t, err)
				assert.Error(t, c.MaxCurrent(6))
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.wantMin, lower)
				assert.Equal(t, tc.wantMax, upper)
			}
		})
	}
	for _, key := range []string{"var", "mca", "ama"} {
		t.Run("missing "+key, func(t *testing.T) {
			var values map[string]any
			require.NoError(t, json.Unmarshal([]byte(goELocalFixture), &values))
			delete(values, key)
			body, err := json.Marshal(values)
			require.NoError(t, err)
			c := goELocalCharger(t, goELocalServer(t, string(body), nil))
			assert.ErrorContains(t, c.MaxCurrent(6), "missing charger current limits")
		})
	}
}

func TestGoELocalCommandFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"rejected", 200, `{"frc":false}`},
		{"device message", 200, `{"frc":"access denied"}`},
		{"missing acknowledgement", 200, `{}`},
		{"other key", 200, `{"amp":true}`},
		{"null acknowledgement", 200, `{"frc":null}`},
		{"string acknowledgement", 200, `{"frc":"true"}`},
		{"malformed JSON", 200, `{"frc":`},
		{"empty response", 200, ""},
		{"no content", 204, ""},
		{"HTTP failure", 500, `{"frc":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := goELocalServer(t, goELocalFixture, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.body)
			})
			c := goELocalCharger(t, srv)
			assert.Error(t, c.Enable(true))
		})
	}
}

func TestGoELocalCacheInvalidation(t *testing.T) {
	for _, acknowledgement := range []string{`{"frc":true}`, `{"frc":false}`, `{"frc":`} {
		t.Run(acknowledgement, func(t *testing.T) {
			var reads atomic.Int32
			var enabled atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/set" {
					enabled.Store(true)
					fmt.Fprint(w, acknowledgement)
					return
				}
				reads.Add(1)
				fmt.Fprintf(w, `{"car":1,"err":0,"alw":%t,"psm":0}`, enabled.Load())
			}))
			t.Cleanup(srv.Close)
			c, err := NewGoELocalFromConfig(t.Context(), map[string]any{"uri": srv.URL, "cache": "1h"})
			require.NoError(t, err)
			for range 3 {
				got, err := c.Enabled()
				require.NoError(t, err)
				assert.False(t, got)
			}
			assert.EqualValues(t, 1, reads.Load())
			err = c.Enable(true)
			if acknowledgement == `{"frc":true}` {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
			got, err := c.Enabled()
			require.NoError(t, err)
			assert.True(t, got)
			assert.EqualValues(t, 2, reads.Load())
		})
	}
}

func TestGoELocalInvalidStatus(t *testing.T) {
	for _, body := range []string{"", `{}`, `null`, `{"car":`, `{"car":null,"err":0,"alw":false,"psm":0}`, `{"car":1,"err":null,"alw":false,"psm":0}`, `{"car":1,"err":0,"alw":null,"psm":0}`, `{"car":1,"err":0,"alw":false}`, `{"car":1,"err":0,"alw":1,"psm":0}`} {
		t.Run(body, func(t *testing.T) {
			srv := goELocalServer(t, body, nil)
			_, err := NewGoELocalFromConfig(t.Context(), map[string]any{"uri": srv.URL})
			assert.Error(t, err)
		})
	}
}

func TestGoELocalMissingMeasurements(t *testing.T) {
	for _, extra := range []string{"", `,"nrg":[],"eto":null`, `,"nrg":[230,231],"eto":-1`, `,"nrg":[null,null,null,null,null,null,null,null,null,null,null,null]`} {
		t.Run(extra, func(t *testing.T) {
			body := `{"car":1,"err":0,"alw":false,"psm":0` + extra + `}`
			c := goELocalCharger(t, goELocalServer(t, body, nil))
			_, err := c.CurrentPower()
			assert.ErrorIs(t, err, api.ErrNotAvailable)
			_, err = c.TotalEnergy()
			assert.ErrorIs(t, err, api.ErrNotAvailable)
			_, _, _, err = c.Currents()
			assert.ErrorIs(t, err, api.ErrNotAvailable)
			_, _, _, err = c.Voltages()
			assert.ErrorIs(t, err, api.ErrNotAvailable)
		})
	}
}

func TestGoELocalInvalidConfig(t *testing.T) {
	for _, uri := range []string{"", " ", "http://", "ftp://charger", "http://charger/api/status", "http://charger?key=value", "http://charger#fragment", "http://user:password@charger"} {
		t.Run(uri, func(t *testing.T) {
			_, err := NewGoELocalFromConfig(t.Context(), map[string]any{"uri": uri})
			assert.Error(t, err)
		})
	}
	_, err := NewGoELocalFromConfig(t.Context(), map[string]any{"uri": "http://charger", "cache": "-1s"})
	assert.ErrorContains(t, err, "cache")
	_, err = NewGoELocalFromConfig(t.Context(), map[string]any{"uri": "http://charger", "unknown": true})
	assert.Error(t, err)
}

func TestGoELocalTransportFailure(t *testing.T) {
	srv := goELocalServer(t, goELocalFixture, nil)
	c := goELocalCharger(t, srv)
	srv.Close()
	_, err := c.Status()
	assert.Error(t, err)
	_, err = c.Enabled()
	assert.Error(t, err)
	_, err = c.CurrentPower()
	assert.Error(t, err)
	assert.Error(t, c.Enable(true))
}

func TestGoELocalCancellation(t *testing.T) {
	srv := goELocalServer(t, goELocalFixture, nil)
	ctx, cancel := context.WithCancel(t.Context())
	c, err := NewGoELocalFromConfig(ctx, map[string]any{"uri": srv.URL, "cache": "0s"})
	require.NoError(t, err)
	cancel()
	_, err = c.Status()
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, c.Enable(true), context.Canceled)
}
