package charger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
)

// GoELocal controls Gemini chargers using the public local HTTP API v2.
type GoELocal struct {
	ctx    context.Context
	client *request.Helper
	uri    string
	status util.Cacheable[goELocalStatus]
}

type goELocalStatus struct {
	Car          *int       `json:"car"`
	Error        *int       `json:"err"`
	Allowed      *bool      `json:"alw"`
	Energy       *float64   `json:"eto"`
	Measurements []*float64 `json:"nrg"`
	Variant      *int       `json:"var"`
	MinCurrent   *int       `json:"mca"`
	MaxCurrent   *int       `json:"ama"`
	PhaseMode    *int       `json:"psm"`
}

func init() {
	registry.AddCtx("go-e-local", NewGoELocalFromConfig)
}

// NewGoELocalFromConfig creates a local go-e HTTP API v2 charger.
func NewGoELocalFromConfig(ctx context.Context, other map[string]any) (api.Charger, error) {
	cc := struct {
		URI   string
		Cache time.Duration
	}{Cache: time.Second}
	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cc.URI) == "" {
		return nil, errors.New("missing uri")
	}
	if cc.Cache < 0 {
		return nil, errors.New("cache must not be negative")
	}

	u, err := url.Parse(util.DefaultScheme(cc.URI, "http"))
	if err != nil {
		return nil, fmt.Errorf("invalid uri: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("uri must be an HTTP(S) charger address without credentials, path, query or fragment")
	}

	c := &GoELocal{
		ctx:    ctx,
		client: request.NewHelper(util.NewLogger("go-e-local")),
		uri:    strings.TrimRight(u.String(), "/"),
	}
	c.status = util.ResettableCached(c.readStatus, cc.Cache)
	if _, err := c.status.Get(); err != nil {
		return nil, fmt.Errorf("local HTTP API v2: %w", err)
	}
	return c, nil
}

func (c *GoELocal) get(path string, query url.Values, result any) error {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.uri+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	return c.client.DoJSON(req, result)
}

func (c *GoELocal) readStatus() (goELocalStatus, error) {
	var res goELocalStatus
	err := c.get("/api/status", url.Values{"filter": {"car,err,alw,eto,nrg,var,mca,ama,psm"}}, &res)
	if err == nil && (res.Car == nil || res.Error == nil || res.Allowed == nil || res.PhaseMode == nil) {
		err = errors.New("incomplete status: expected car, err, alw and psm; enable HTTP API v2 in the go-e app")
	}
	return res, err
}

func (c *GoELocal) set(key string, value int64) error {
	// A command may reach the charger even if its response is lost.
	defer c.status.Reset()
	var res map[string]json.RawMessage
	if err := c.get("/api/set", url.Values{key: {strconv.FormatInt(value, 10)}}, &res); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}
	if result, ok := res[key]; !ok || strings.TrimSpace(string(result)) != "true" {
		return fmt.Errorf("set %s: charger did not acknowledge command (%s)", key, res[key])
	}
	return nil
}

var _ api.Charger = (*GoELocal)(nil)

// Status implements the api.Charger interface.
func (c *GoELocal) Status() (api.ChargeStatus, error) {
	s, err := c.status.Get()
	if err != nil {
		return api.StatusNone, err
	}
	if *s.Error != 0 {
		return api.StatusNone, fmt.Errorf("charger error: %d", *s.Error)
	}
	switch *s.Car {
	case 1:
		return api.StatusA, nil
	case 2:
		return api.StatusC, nil
	case 3, 4:
		return api.StatusB, nil
	default:
		return api.StatusNone, fmt.Errorf("invalid car state: %d", *s.Car)
	}
}

// Enabled implements the api.Charger interface.
func (c *GoELocal) Enabled() (bool, error) {
	s, err := c.status.Get()
	if err != nil {
		return false, err
	}
	return *s.Allowed, nil
}

// Enable implements the api.Charger interface.
func (c *GoELocal) Enable(enable bool) error {
	state := int64(1)
	if enable {
		state = 2
	}
	return c.set("frc", state)
}

var _ api.CurrentLimiter = (*GoELocal)(nil)

// GetMinMaxCurrent implements the api.CurrentLimiter interface.
func (c *GoELocal) GetMinMaxCurrent() (float64, float64, error) {
	s, err := c.status.Get()
	if err != nil {
		return 0, 0, err
	}
	if s.Variant == nil || s.MinCurrent == nil || s.MaxCurrent == nil {
		return 0, 0, errors.New("missing charger current limits (var, mca, ama)")
	}
	var hardwareMax int
	switch *s.Variant {
	case 11:
		hardwareMax = 16
	case 22:
		hardwareMax = 32
	default:
		return 0, 0, fmt.Errorf("unknown charger variant: %d", *s.Variant)
	}
	lower, upper := max(6, *s.MinCurrent), min(hardwareMax, *s.MaxCurrent)
	if lower > upper {
		return 0, 0, fmt.Errorf("invalid charger current limits: %d-%d A", lower, upper)
	}
	return float64(lower), float64(upper), nil
}

// MaxCurrent implements the api.Charger interface.
func (c *GoELocal) MaxCurrent(current int64) error {
	lower, upper, err := c.GetMinMaxCurrent()
	if err != nil {
		return err
	}
	if current < int64(lower) || current > int64(upper) {
		return fmt.Errorf("current must be between %.0f and %.0f A: %d", lower, upper, current)
	}
	return c.set("amp", current)
}

var _ api.PhaseSwitcher = (*GoELocal)(nil)

// Phases1p3p implements the api.PhaseSwitcher interface.
func (c *GoELocal) Phases1p3p(phases int) error {
	switch phases {
	case 1:
		return c.set("psm", 1)
	case 3:
		return c.set("psm", 2)
	default:
		return fmt.Errorf("phases must be 1 or 3: %d", phases)
	}
}

func goELocalReading(value *float64) (float64, error) {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return 0, api.ErrNotAvailable
	}
	return *value, nil
}

func (c *GoELocal) measurements(start, count int) ([]float64, error) {
	s, err := c.status.Get()
	if err != nil {
		return nil, err
	}
	if len(s.Measurements) < start+count {
		return nil, fmt.Errorf("incomplete nrg measurements: %w", api.ErrNotAvailable)
	}
	values := make([]float64, count)
	for i := range values {
		values[i], err = goELocalReading(s.Measurements[start+i])
		if err != nil {
			return nil, fmt.Errorf("nrg[%d]: %w", start+i, err)
		}
	}
	return values, nil
}

var _ api.Meter = (*GoELocal)(nil)

// CurrentPower implements the api.Meter interface.
func (c *GoELocal) CurrentPower() (float64, error) {
	values, err := c.measurements(11, 1)
	if err != nil {
		return 0, err
	}
	return values[0], nil
}

var _ api.MeterEnergy = (*GoELocal)(nil)

// TotalEnergy implements the api.MeterEnergy interface.
func (c *GoELocal) TotalEnergy() (float64, error) {
	s, err := c.status.Get()
	if err != nil {
		return 0, err
	}
	value, err := goELocalReading(s.Energy)
	return value / 1000, err
}

var _ api.PhaseCurrents = (*GoELocal)(nil)

// Currents implements the api.PhaseCurrents interface.
func (c *GoELocal) Currents() (float64, float64, float64, error) {
	values, err := c.measurements(4, 3)
	if err != nil {
		return 0, 0, 0, err
	}
	return values[0], values[1], values[2], nil
}

var _ api.PhaseVoltages = (*GoELocal)(nil)

// Voltages implements the api.PhaseVoltages interface.
func (c *GoELocal) Voltages() (float64, float64, float64, error) {
	values, err := c.measurements(0, 3)
	if err != nil {
		return 0, 0, 0, err
	}
	return values[0], values[1], values[2], nil
}
