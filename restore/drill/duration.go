package drill

import (
	"encoding/json"
	"math"
	"time"
)

// Duration serialises as seconds so the report stays a plain number for
// whatever reads it, and prints rounded for whoever reads the logs.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(math.Round(time.Duration(d).Seconds()*1000) / 1000)
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s float64
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*d = Duration(time.Duration(s * float64(time.Second)))
	return nil
}

func (d Duration) String() string {
	v := time.Duration(d)
	switch {
	case v >= time.Hour:
		return v.Round(time.Minute).String()
	case v >= time.Second:
		return v.Round(time.Second).String()
	default:
		return v.Round(time.Millisecond).String()
	}
}
