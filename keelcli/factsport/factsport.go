// Package factsport declares the facts about a database that keelcli's doctor and console header
// read: the connection, server settings and extensions, the backends holding locks or running long
// queries, and the environment mark.
//
// Template state and the schema version come from the migration ports, so they are not repeated
// here. Each interface covers one use; Facts joins them for App.
package factsport

import (
	"context"
	"time"

	"github.com/emoss08/keel/keelcli/seedport"
)

// Connection describes the database keelcli talks to. DSN may carry a password; keelcli redacts it
// before showing it.
type Connection struct {
	DSN           string
	Database      string
	Host          string
	Port          int
	User          string
	ServerVersion string
	ServerNum     int
	Latency       time.Duration
}

// Setting is one server setting.
type Setting struct {
	Name  string
	Value string
}

// Extension is one installed extension.
type Extension struct {
	Name    string
	Version string
}

// Backend is one server process from pg_stat_activity. Mode and Relation describe the lock it
// holds or waits for; BlockedBy lists the backends it waits on.
type Backend struct {
	PID         int32
	Application string
	User        string
	State       string
	Mode        string
	Relation    string
	Age         time.Duration
	Query       string
	Waiting     bool
	BlockedBy   []int32
}

// Activity is the backends worth a look: those holding locks others wait for, and queries running
// longer than the threshold Activity was asked for.
type Activity struct {
	LockHolders []Backend
	LongQueries []Backend
}

// Connector connects and describes the connection.
type Connector interface {
	Connection(ctx context.Context) (Connection, error)
}

// Inspector reads server settings and installed extensions.
type Inspector interface {
	Settings(ctx context.Context, names ...string) ([]Setting, error)
	Extensions(ctx context.Context) ([]Extension, error)
}

// Monitor reads current lock holders and queries running longer than longer.
type Monitor interface {
	Activity(ctx context.Context, longer time.Duration) (Activity, error)
}

// EnvironmentReader reads the database's environment mark without the seed engine, so commands of
// an application without seeds can still refuse a production database.
type EnvironmentReader interface {
	Environment(ctx context.Context) (seedport.Environment, error)
}

// Facts is every facts port.
type Facts interface {
	Connector
	Inspector
	Monitor
	EnvironmentReader
}
