package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/quantonganh/geohash"
	"github.com/quantonganh/httperror"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

const dbPath = "./db/nearby_cities.db"

//go:embed templates/*.html
var htmlFS embed.FS

//go:embed static
var staticFS embed.FS

//go:embed worldcities.csv
var worldCitiesCSV string

func main() {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		fmt.Printf("Error creating directories: %v\n", err)
		return
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		log.Fatal(err)
	}

	if err := prepare(db); err != nil {
		log.Fatal(err)
	}

	zlog := zerolog.New(os.Stdout).With().
		Timestamp().
		Logger()

	r := httperror.NewRouter()
	r.Use(hlog.NewHandler(zlog))
	r.Use(hlog.AccessHandler(func(r *http.Request, status, size int, duration time.Duration) {
		hlog.FromRequest(r).Info().
			Str("method", r.Method).
			Stringer("url", r.URL).
			Int("status", status).
			Int("size", size).
			Dur("duration", duration).
			Msg("")
	}))
	r.Use(httperror.RealIPHandler("ip"))
	r.Use(hlog.UserAgentHandler("user_agent"))
	r.Use(hlog.RefererHandler("referer"))
	r.Use(hlog.RequestIDHandler("req_id", "Request-Id"))
	r.Add("/static/", func(w http.ResponseWriter, r *http.Request) error {
		http.FileServer(http.FS(staticFS)).ServeHTTP(w, r)
		return nil
	})

	tmpl, err := template.New("index.html").ParseFS(htmlFS, "templates/*.html")
	if err != nil {
		log.Fatal(err)
	}

	r.Add("/", indexHandler(db, tmpl))
	r.Add("/search", searchHandler(db, tmpl))
	server := httperror.NewServer(r.Mux, ":8080")

	go func() {
		fmt.Printf("Server is listening on port %s...\n", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("\nShutting down server...")
	if err := server.Shutdown(context.Background()); err != nil {
		log.Fatal(err)
	}

	if err := db.Close(); err != nil {
		log.Fatal(err)
	}

	fmt.Println("Server has stopped.")
}

func prepare(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS migrations (name TEXT PRIMARY KEY);`); err != nil {
		return fmt.Errorf("error creating migrations table: %w", err)
	}

	var migrationApplied bool
	err := db.QueryRow(`
		SELECT EXISTS (SELECT 1 from migrations WHERE name = 'cities_table')
	`).Scan(&migrationApplied)
	if err != nil {
		return fmt.Errorf("error checking migration status: %w", err)
	}

	if !migrationApplied {
		token := os.Getenv("IP2LOCATION_TOKEN")
		resp, err := http.Get(fmt.Sprintf("https://www.ip2location.com/download/?token=%s&file=DB3LITE", token))
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		ip2LocationFile, err := os.CreateTemp("", "IP2LOCATION-LITE-DB3*.csv")
		if err != nil {
			return fmt.Errorf("error creating ip2Location file: %w", err)
		}
		defer os.Remove(ip2LocationFile.Name())

		_, err = io.Copy(ip2LocationFile, resp.Body)
		if err != nil {
			return err
		}

		worldCitiesFile, err := os.CreateTemp("", "worldcities*.csv")
		if err != nil {
			return fmt.Errorf("error creating temp file: %w", err)
		}
		defer os.Remove(worldCitiesFile.Name())

		if _, err := worldCitiesFile.Write([]byte(worldCitiesCSV)); err != nil {
			return fmt.Errorf("error writing the embedded CSV content: %w", err)
		}

		_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS ip2location (
			start_ip TEXT,
			end_ip TEXT,
			iso2 TEXT,
			country TEXT,
			city TEXT,
			region TEXT,
			lat TEXT,
			lng TEXT
		);
		`)
		if err != nil {
			return fmt.Errorf("error creating ip2location table: %w", err)
		}

		cmd := exec.Command("sqlite3", dbPath, "-cmd", ".mode csv", fmt.Sprintf(".import %s ip2location", ip2LocationFile.Name()))
		_, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("error importing CSV data into ip2location table: %w", err)
		}

		cmd = exec.Command("sqlite3", dbPath, "-cmd", ".mode csv", fmt.Sprintf(".import %s cities", worldCitiesFile.Name()))
		_, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("error importing CSV data into cities table: %w", err)
		}

		_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS geospatial_index (
			geohash TEXT,
			city_id INTEGER UNIQUE,
			FOREIGN KEY(city_id) REFERENCES cities(id)
		)
		`)
		if err != nil {
			return fmt.Errorf("error creating goepatial_index table: %w", err)
		}

		_, err = db.Exec(`
			CREATE VIRTUAL TABLE cities_fts USING fts5(
				city,
				city_ascii,
				lat,
				lng,
				country,
				iso2,
				iso3,
				admin_name,
				capital,
				population,
				id,
				content='cities',
				tokenize='unicode61'
			);
		`)
		if err != nil {
			return fmt.Errorf("error creating cities_fts table: %w", err)
		}

		_, err = db.Exec(`
			INSERT INTO cities_fts(city, city_ascii, lat, lng, country, iso2, iso3, admin_name, capital, population, id)
			SELECT city, city_ascii, lat, lng, country, iso2, iso3, admin_name, capital, population, id FROM cities;
		`)
		if err != nil {
			return fmt.Errorf("error populating the virtual table cities_fts: %w", err)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("error starting transaction: %w", err)
		}
		defer tx.Rollback()

		rows, err := tx.Query(`
		SELECT id, lat, lng FROM cities
		`)
		if err != nil {
			return fmt.Errorf("error selecting lat, lng from cities table: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				id       int
				lat, lng float64
			)
			if err := rows.Scan(&id, &lat, &lng); err != nil {
				return fmt.Errorf("error scanning: %w", err)
			}

			gh := geohash.Encode(lat, lng)

			_, err = tx.Exec(`
			INSERT INTO geospatial_index (geohash, city_id)
			VALUES (?, ?)
		`, gh, id)
			if err != nil {
				return fmt.Errorf("error inserting into geospatial_index: %w", err)
			}
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("error during iteration: %w", err)
		}

		_, err = tx.Exec("INSERT INTO migrations (name) VALUES ('cities_table')")
		if err != nil {
			return fmt.Errorf("error marking migration as applied: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("error committing transaction: %w", err)
		}
	}

	return nil
}

type IP2LocationData struct {
	StartIP uint32
	EndIP   uint32
	Country string
	Region  string
	City    string
}

type city struct {
	City       string
	CityAscii  string
	Lat        float64
	Lng        float64
	Country    string
	Iso2       string
	Iso3       string
	AdminName  string
	Capital    string
	Population string
	ID         string
	Geohash    string
	Distance   float64
}

type PageData struct {
	FromCity     string
	Radius       string
	NearbyCities []city
	Message      string
}

func indexHandler(db *sql.DB, tmpl *template.Template) httperror.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		ip, err := httperror.GetIP(r)
		if err != nil {
			return tmpl.ExecuteTemplate(w, "base", PageData{})
		}

		ipInteger, err := ipToInteger(ip)
		if err != nil {
			return tmpl.ExecuteTemplate(w, "base", PageData{})
		}

		row := db.QueryRow(`
			SELECT start_ip, end_ip, city, region, country FROM ip2location WHERE ? BETWEEN start_ip AND end_ip 
			`, ipInteger)
		var ip2Loc IP2LocationData
		if err = row.Scan(&ip2Loc.StartIP, &ip2Loc.EndIP, &ip2Loc.City, &ip2Loc.Region, &ip2Loc.Country); err != nil {
			return tmpl.ExecuteTemplate(w, "base", PageData{})
		}

		cities, err := findNearbyCities(db, fmt.Sprintf("%s %s", ip2Loc.City, ip2Loc.Region))
		if err != nil {
			return tmpl.ExecuteTemplate(w, "base", PageData{})
		}

		data := PageData{
			FromCity:     fmt.Sprintf("%s, %s", ip2Loc.City, ip2Loc.Country),
			NearbyCities: cities,
		}

		return tmpl.ExecuteTemplate(w, "base", data)
	}
}

func ipToInteger(ipAddr string) (uint32, error) {
	parsedIP := net.ParseIP(ipAddr)
	if parsedIP == nil {
		return 0, fmt.Errorf("invalid IP address: %s", ipAddr)
	}

	ipBytes := parsedIP.To4()
	if ipBytes == nil {
		return 0, fmt.Errorf("not an IPv4 address: %s", ipAddr)
	}

	ipInteger := uint32(ipBytes[0])<<24 | uint32(ipBytes[1])<<16 | uint32(ipBytes[2])<<8 | uint32(ipBytes[3])

	return ipInteger, nil
}

func searchHandler(db *sql.DB, tmpl *template.Template) httperror.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		fromCity := r.FormValue("city")
		nearbyCities, err := findNearbyCities(db, fromCity)
		if err != nil {
			if err == sql.ErrNoRows {
				data := PageData{
					Message: "No matching city found.",
				}

				return tmpl.ExecuteTemplate(w, "base", data)
			} else {
				return err
			}
		}

		data := PageData{
			FromCity:     fromCity,
			NearbyCities: nearbyCities,
		}

		return tmpl.ExecuteTemplate(w, "base", data)
	}
}

func findNearbyCities(db *sql.DB, fromCity string) ([]city, error) {
	normalizedCity := normalizeQuery(fromCity)
	row := db.QueryRow(`
			SELECT city, lat, lng, country FROM cities_fts WHERE cities_fts MATCH ? 
			`, normalizedCity)
	var c city
	err := row.Scan(&c.City, &c.Lat, &c.Lng, &c.Country)
	if err != nil {
		return nil, err
	}

	hash := geohash.Encode(c.Lat, c.Lng)
	length := geohash.EstimateLengthRequired(100)
	rows, err := db.Query(`
			SELECT c.city, c.lat, c.lng, c.admin_name, c.country, g.geohash
			FROM cities c JOIN geospatial_index g ON g.city_id = c.id
			WHERE g.geohash LIKE ?;
		`, fmt.Sprintf("%s%%", hash[:length]))
	if err != nil {
		return nil, err
	}

	cities := make([]city, 0)
	for rows.Next() {
		var toCity city
		if err := rows.Scan(&toCity.City, &toCity.Lat, &toCity.Lng, &toCity.AdminName, &toCity.Country, &toCity.Geohash); err != nil {
			return nil, err
		}

		distance := geohash.Distance(c.Lat, c.Lng, toCity.Lat, toCity.Lng)
		toCity.Distance = math.Round(distance*100) / 100
		cities = append(cities, toCity)
	}

	sort.Slice(cities, func(i, j int) bool {
		return cities[i].Distance < cities[j].Distance
	})

	return cities, nil
}

func normalizeQuery(query string) string {
	re := regexp.MustCompile(`[\p{P}]`)
	return re.ReplaceAllString(query, "")
}

func errorHandler(handler httperror.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := handler(w, r)
		if err != nil {
			// Handle the error and send an appropriate response
			fmt.Println("Error:", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}
