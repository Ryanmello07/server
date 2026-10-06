package main

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/maxmind/mmdbwriter"
	maxminddb "github.com/oschwald/maxminddb-golang/v2"
)

const vaultDir = "./beta-vault"

var (
	// The server reads exactly mmdb/geolite2.mmdb and refuses any database
	// type other than GeoLite2-City (ip.go loadIpDb).
	mmdbPath = filepath.Join(vaultDir, "config", "mmdb", "geolite2.mmdb")
	// Where this script wrote the same GeoLite2-City data before the server
	// adopted the geolite2.mmdb name. Reused instead of downloading again.
	legacyMmdbPath = filepath.Join(vaultDir, "config", "mmdb", "ip-ipinfo.mmdb")
	arinPath       = filepath.Join(vaultDir, "config", "arindb", "arin.mmdb")
)

const geoLite2CityType = "GeoLite2-City"
const arinDbType = "urnetwork arindb"

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

	if _, err := os.Stat(mmdbPath); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(legacyMmdbPath); err == nil {
			if err := linkOrCopy(legacyMmdbPath, mmdbPath); err != nil {
				return fmt.Errorf("reuse %s: %w", legacyMmdbPath, err)
			}
			fmt.Printf("Reused %s as %s\n", legacyMmdbPath, mmdbPath)
		} else if err := downloadGeoLite(); err != nil {
			return fmt.Errorf("download GeoLite2-City: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat mmdb: %w", err)
	} else {
		fmt.Printf("MMDB already exists: %s\n", mmdbPath)
	}
	if err := requireDatabaseType(mmdbPath, geoLite2CityType); err != nil {
		return err
	}

	// The ARIN database is synthetic on beta, so it is rewritten whenever it
	// is missing or is the legacy stub. That stub mapped every address to an
	// org registered in "us"; the server now turns a registration country that
	// differs from the address's location into ARIN risk, a HARD exclusion, so
	// the legacy stub would exclude every provider located outside the US.
	legacy, err := isLegacyArinStub(arinPath)
	switch {
	case errors.Is(err, os.ErrNotExist) || legacy:
		if err := writeArinDB(); err != nil {
			return fmt.Errorf("generate ARIN mmdb: %w", err)
		}
	case err != nil:
		return fmt.Errorf("inspect arin mmdb: %w", err)
	default:
		fmt.Printf("ARIN MMDB already exists: %s\n", arinPath)
	}
	return nil
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func requireDatabaseType(path, want string) error {
	db, err := maxminddb.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	if got := db.Metadata.DatabaseType; got != want {
		return fmt.Errorf("%s has database type %q; the server only accepts %q", path, got, want)
	}
	return nil
}

// A record for an address outside the US is the tell: the empty stub has no
// records at all, and the legacy stub answered every address with "us".
func isLegacyArinStub(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		return false, err
	}
	db, err := maxminddb.Open(path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	if db.Metadata.DatabaseType != arinDbType {
		return false, fmt.Errorf("%s has database type %q, want %q", path, db.Metadata.DatabaseType, arinDbType)
	}
	return db.Lookup(netip.MustParseAddr("81.2.69.142")).Found(), nil
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

// writeArinDB writes an ARIN database with the server's expected type and no
// records. A lookup then finds nothing, which the server treats as an evaluated
// unknown: no ARIN risk and no non-quality mark for any provider.
func writeArinDB() error {
	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: arinDbType,
		Languages:    []string{"en"},
		Description: map[string]string{
			"en": "URnetwork beta ARIN-like database (no records)",
		},
		IPVersion:  6,
		RecordSize: 24,
	})
	if err != nil {
		return fmt.Errorf("create mmdb writer: %w", err)
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
	fmt.Printf("Wrote %s (%d bytes, no records)\n", arinPath, written)
	return nil
}
