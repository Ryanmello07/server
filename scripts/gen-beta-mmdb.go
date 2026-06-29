package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

const vaultDir = "./beta-vault"

var (
	mmdbPath = filepath.Join(vaultDir, "config", "mmdb", "ip-ipinfo.mmdb")
	arinPath = filepath.Join(vaultDir, "config", "arindb", "arin.mmdb")
)

// geoLiteURL uses the free wp-statistics mirror of GeoLite2-City.
const geoLiteURL = "https://github.com/wp-statistics/GeoLite2-City/raw/master/GeoLite2-City.mmdb.gz"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "gen-beta-mmdb: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	for _, dir := range []string{filepath.Dir(mmdbPath), filepath.Dir(arinPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	if _, err := os.Stat(mmdbPath); os.IsNotExist(err) {
		if err := downloadGeoLite(); err != nil {
			return fmt.Errorf("download GeoLite2-City: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat mmdb: %w", err)
	} else {
		fmt.Printf("MMDB already exists: %s\n", mmdbPath)
	}

	if _, err := os.Stat(arinPath); os.IsNotExist(err) {
		if err := writeArinDB(); err != nil {
			return fmt.Errorf("generate ARIN mmdb: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat arin mmdb: %w", err)
	} else {
		fmt.Printf("ARIN MMDB already exists: %s\n", arinPath)
	}

	return nil
}

func downloadGeoLite() error {
	fmt.Printf("Downloading %s ...\n", geoLiteURL)
	resp, err := http.Get(geoLiteURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	f, err := os.Create(mmdbPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", mmdbPath, err)
	}
	defer f.Close()

	written, err := io.Copy(f, gz)
	if err != nil {
		return fmt.Errorf("write mmdb: %w", err)
	}
	fmt.Printf("Wrote %s (%d bytes)\n", mmdbPath, written)
	return nil
}

func writeArinDB() error {
	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "urnetwork arindb",
		Languages:    []string{"en"},
		Description: map[string]string{
			"en": "URnetwork beta ARIN-like database",
		},
		IPVersion:  6,
		RecordSize: 24,
	})
	if err != nil {
		return fmt.Errorf("create mmdb writer: %w", err)
	}

	record := mmdbtype.Map{
		"org_country_codes": mmdbtype.Slice{mmdbtype.String("us")},
	}

	for _, cidr := range []string{"0.0.0.0/0", "::/0"} {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("parse %s: %w", cidr, err)
		}
		if err := tree.Insert(ipNet, record); err != nil {
			return fmt.Errorf("insert %s: %w", cidr, err)
		}
	}

	f, err := os.Create(arinPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", arinPath, err)
	}
	defer f.Close()

	written, err := tree.WriteTo(f)
	if err != nil {
		return fmt.Errorf("write arin mmdb: %w", err)
	}
	fmt.Printf("Wrote %s (%d bytes)\n", arinPath, written)
	return nil
}
