// Package dbgen is sqlc's code for schema.sql and queries/*.sql. The build generates it.
package dbgen

//go:generate env CGO_ENABLED=0 go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.28.0 generate -f ../../../sqlc.yaml
