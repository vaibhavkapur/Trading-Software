package unit

import (
	"errors"
	"testing"

	"mdp/src/fixed"
)

func unit(t *testing.T, s string) fixed.Unit {
	t.Helper()
	u, err := fixed.ParseUnit(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUnitsAreExact(t *testing.T) {
	cases := []struct {
		unit, value string
		want        int64
	}{
		{"0.01", "101.25", 10125},
		{"0.01", "101.250000", 10125}, // trailing zeros do not change the value
		{"0.01", "0", 0},
		{"0.01", "-1.5", -150},
		{"0.05", "0.15", 3}, // 0.15/0.05 is 2.9999999999999996 in binary floating point
		{"0.005", "75.015", 15003},
		{"0.0001", "1.0875", 10875},
		{"1", "100", 100},
		{"0.001", "0.123", 123},
		{"25", "1000", 40},
		{"0.1", "0.3", 3}, // 0.1*3 != 0.3 in binary floating point
		{"0.00000001", "92233720368.54775807", 9223372036854775807},
	}
	for _, c := range cases {
		got, err := unit(t, c.unit).Units(c.value)
		if err != nil || got != c.want {
			t.Errorf("%s / %s = %d, %v; want %d", c.value, c.unit, got, err, c.want)
		}
	}
}

func TestUnitsRefuseWhatCannotBeRepresented(t *testing.T) {
	cases := []struct {
		unit, value string
		want        error
	}{
		{"0.01", "101.255", fixed.ErrNotMultiple},
		{"0.05", "0.12", fixed.ErrNotMultiple},
		{"0.01", "0.0000000001", fixed.ErrNotMultiple},
		{"1", "0.5", fixed.ErrNotMultiple},
		{"0.00000001", "92233720368.54775808", fixed.ErrOverflow},
		{"0.01", "", fixed.ErrSyntax},
		{"0.01", "1e3", fixed.ErrSyntax},
		{"0.01", "1E-2", fixed.ErrSyntax},
		{"0.01", " 1.00", fixed.ErrSyntax},
		{"0.01", "1.00 ", fixed.ErrSyntax},
		{"0.01", "+1.00", fixed.ErrSyntax},
		{"0.01", "1,000.00", fixed.ErrSyntax},
		{"0.01", ".5", fixed.ErrSyntax},
		{"0.01", "5.", fixed.ErrSyntax},
		{"0.01", "1.2.3", fixed.ErrSyntax},
		{"0.01", "--1", fixed.ErrSyntax},
		{"0.01", "NaN", fixed.ErrSyntax},
		{"0.01", "Inf", fixed.ErrSyntax},
		{"0.01", "0x10", fixed.ErrSyntax},
		{"0.01", "١٢٣", fixed.ErrSyntax},                                       // non-ASCII digits
		{"0.01", "11111111111111111111111111111111111111111", fixed.ErrSyntax}, // 41 characters
	}
	for _, c := range cases {
		got, err := unit(t, c.unit).Units(c.value)
		if !errors.Is(err, c.want) {
			t.Errorf("%q / %s = %d, %v; want %v", c.value, c.unit, got, err, c.want)
		}
	}
}

func TestParseUnitRequiresAPositiveDecimal(t *testing.T) {
	for _, s := range []string{"", "0", "0.00", "-0.01", "abc", "1e-2"} {
		if _, err := fixed.ParseUnit(s); !errors.Is(err, fixed.ErrBadUnit) {
			t.Errorf("ParseUnit(%q) = %v, want ErrBadUnit", s, err)
		}
	}
}

func TestFormatIsTheInverseOfUnits(t *testing.T) {
	for _, c := range []struct {
		unit  string
		units int64
		want  string
	}{
		{"0.01", 10125, "101.25"},
		{"0.01", 10100, "101"},
		{"0.01", 5, "0.05"},
		{"0.01", 0, "0"},
		{"0.01", -150, "-1.5"},
		{"0.005", 15003, "75.015"},
		{"25", 40, "1000"},
		{"0.001", 1, "0.001"},
	} {
		u := unit(t, c.unit)
		got := u.Format(c.units)
		if got != c.want {
			t.Errorf("Format(%d of %s) = %q, want %q", c.units, c.unit, got, c.want)
		}
		if back, err := u.Units(got); err != nil || back != c.units {
			t.Errorf("Units(Format(%d)) = %d, %v", c.units, back, err)
		}
	}
}

func TestFromScaled(t *testing.T) {
	for _, c := range []struct {
		n     int64
		scale int
		want  string
	}{
		{1012500, 4, "101.25"}, {1, 4, "0.0001"}, {0, 4, "0"}, {-5, 4, "-0.0005"},
		{10000, 4, "1"}, {123, 0, "123"}, {9223372036854775807, 4, "922337203685477.5807"},
	} {
		if got := fixed.FromScaled(c.n, c.scale); got != c.want {
			t.Errorf("FromScaled(%d, %d) = %q, want %q", c.n, c.scale, got, c.want)
		}
	}
}

func FuzzUnitsNeverRounds(f *testing.F) {
	for _, s := range []string{"101.25", "0.015", "-3", "1e5", "0.0000001", "7.", "99999999999999999999"} {
		f.Add(s)
	}
	u, _ := fixed.ParseUnit("0.005")
	f.Fuzz(func(t *testing.T, s string) {
		n, err := u.Units(s)
		if err != nil {
			return
		}
		// Whatever was accepted must convert back to the same count.
		if back, err := u.Units(u.Format(n)); err != nil || back != n {
			t.Fatalf("Units(%q) = %d but Format gives %q → %d, %v", s, n, u.Format(n), back, err)
		}
	})
}
