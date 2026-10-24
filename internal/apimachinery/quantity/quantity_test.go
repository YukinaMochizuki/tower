package quantity_test

import (
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/apimachinery/quantity"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want string // as a big.Rat fraction
	}{
		{"0", "0/1"},
		{"2", "2/1"},
		{"+2", "2/1"},
		{"-2", "-2/1"},
		{"1.5", "3/2"},
		{".5", "1/2"},
		{"1.", "1/1"},
		{"007", "7/1"},
		{"500m", "1/2"},
		{"2000m", "2/1"},
		{"1m", "1/1000"},
		{"1Ki", "1024/1"},
		{"1Mi", "1048576/1"},
		{"1Gi", "1073741824/1"},
		{"1Ti", "1099511627776/1"},
		{"1Pi", "1125899906842624/1"},
		{"1Ei", "1152921504606846976/1"},
		{"1.5Gi", "1610612736/1"},
		{"1k", "1000/1"},
		{"1M", "1000000/1"},
		{"1G", "1000000000/1"},
		{"1T", "1000000000000/1"},
		{"1P", "1000000000000000/1"},
		{"1E", "1000000000000000000/1"},
		{"1e3", "1000/1"},
		{"1E3", "1000/1"},
		{"1e+3", "1000/1"},
		{"1e-3", "1/1000"},
		{"2.5e2", "250/1"},
		{"1e0", "1/1"},
		{"1e100", "1" + strings.Repeat("0", 100) + "/1"},
		{"1e-100", "1/1" + strings.Repeat("0", 100)},
	}
	for _, tt := range tests {
		got, err := quantity.Parse(tt.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.in, err)
			continue
		}
		want, _ := new(big.Rat).SetString(tt.want)
		if got.Cmp(want) != 0 {
			t.Errorf("Parse(%q) = %s, want %s", tt.in, got, want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		in, msg string
	}{
		{"", "not a quantity"},
		{"Mi", "not a quantity"},
		{".", "not a quantity"},
		{"+", "not a quantity"},
		{"1.2.3", "not a quantity"},
		{"--1", "not a quantity"},
		{"1/2", "unknown suffix"},
		{" 1", "not a quantity"},
		{"1 ", "unknown suffix"},
		{"1 Mi", "unknown suffix"},
		{"1mi", "unknown suffix"},
		{"1KI", "unknown suffix"},
		{"1K", "unknown suffix"},
		{"1n", "unknown suffix"},
		{"1u", "unknown suffix"},
		{"1MiB", "unknown suffix"},
		{"1e", "unknown suffix"},
		{"1e+", "unknown suffix"},
		{"1e-", "unknown suffix"},
		{"1e1.5", "unknown suffix"},
		{"1e3Mi", "unknown suffix"},
		{"1e--3", "unknown suffix"},
		{"0x10", "unknown suffix"},
		{"1e101", "exponent is out of range"},
		{"1e-101", "exponent is out of range"},
		{"1e99999999999999999999", "exponent is out of range"},
	}
	for _, tt := range tests {
		_, err := quantity.Parse(tt.in)
		if err == nil || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("Parse(%q) = %v, want an error saying %q", tt.in, err, tt.msg)
		}
	}
}

func TestCPU(t *testing.T) {
	tests := []struct {
		in   string
		want int32
	}{
		{"1", 1},
		{"2", 2},
		{"2000m", 2},
		{"1e1", 10},
		{"2147483647", math.MaxInt32},
	}
	for _, tt := range tests {
		if got, err := quantity.CPU(tt.in); err != nil || got != tt.want {
			t.Errorf("CPU(%q) = %d, %v; want %d", tt.in, got, err, tt.want)
		}
	}
	rejects := []struct {
		in, msg string
	}{
		{"500m", "vCPUs are whole"},
		{"1.5", "vCPUs are whole"},
		{"1001m", "vCPUs are whole"},
		{"0", "at least 1"},
		{"0.0", "at least 1"},
		{"-1", "at least 1"},
		{"2147483648", "too large"},
		{"1e30", "too large"},
		{"18446744073709551617", "too large"},
		{"two", "not a quantity"},
	}
	for _, tt := range rejects {
		_, err := quantity.CPU(tt.in)
		if err == nil || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("CPU(%q) = %v, want an error saying %q", tt.in, err, tt.msg)
		}
	}
}

func TestBytes(t *testing.T) {
	tests := []struct {
		in   string
		want uint64
	}{
		{"0", 0},
		{"1", 1},
		{"512Mi", 512 << 20},
		{"512M", 512_000_000},
		{"1.5Ki", 1536},
		{"1000m", 1},
		{"18446744073709551615", math.MaxUint64},
		{"15Ei", 15 << 60},
	}
	for _, tt := range tests {
		if got, err := quantity.Bytes(tt.in); err != nil || got != tt.want {
			t.Errorf("Bytes(%q) = %d, %v; want %d", tt.in, got, err, tt.want)
		}
	}
	rejects := []struct {
		in, msg string
	}{
		{"-1", "negative"},
		{"-1Mi", "negative"},
		{"0.5", "whole number of bytes"},
		{"1500m", "whole number of bytes"},
		{"1.1Ki", "whole number of bytes"},
		{"18446744073709551616", "too large"},
		{"16Ei", "too large"},
		{"1e20", "too large"},
		{"1Zi", "unknown suffix"},
	}
	for _, tt := range rejects {
		_, err := quantity.Bytes(tt.in)
		if err == nil || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("Bytes(%q) = %v, want an error saying %q", tt.in, err, tt.msg)
		}
	}
}

func TestFormat(t *testing.T) {
	if got := quantity.FormatCPU(2); got != "2" {
		t.Errorf("FormatCPU(2) = %q", got)
	}
	tests := []struct {
		f     func(uint64) string
		name  string
		bytes uint64
		want  string
	}{
		{quantity.FormatMemory, "memory", 0, "0Mi"},
		{quantity.FormatMemory, "memory", 1, "1Mi"},
		{quantity.FormatMemory, "memory", quantity.MiB - 1, "1Mi"},
		{quantity.FormatMemory, "memory", quantity.MiB, "1Mi"},
		{quantity.FormatMemory, "memory", quantity.MiB + 1, "2Mi"},
		{quantity.FormatMemory, "memory", 512 << 20, "512Mi"},
		{quantity.FormatMemory, "memory", 512_000_000, "489Mi"},
		{quantity.FormatMemory, "memory", math.MaxUint64, "17592186044416Mi"},
		{quantity.FormatDisk, "disk", 0, "0Gi"},
		{quantity.FormatDisk, "disk", 1, "1Gi"},
		{quantity.FormatDisk, "disk", quantity.GiB - 1, "1Gi"},
		{quantity.FormatDisk, "disk", quantity.GiB, "1Gi"},
		{quantity.FormatDisk, "disk", quantity.GiB + 1, "2Gi"},
		{quantity.FormatDisk, "disk", 20 << 30, "20Gi"},
		{quantity.FormatDisk, "disk", 10_000_000_000, "10Gi"},
		{quantity.FormatDisk, "disk", 11_000_000_000, "11Gi"},
		{quantity.FormatDisk, "disk", math.MaxUint64, "17179869184Gi"},
	}
	for _, tt := range tests {
		if got := tt.f(tt.bytes); got != tt.want {
			t.Errorf("format %s %d = %q, want %q", tt.name, tt.bytes, got, tt.want)
		}
	}
}
