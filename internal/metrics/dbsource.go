package metrics

// DBSource yields the container names that hold databases — the shared MariaDB
// plus every dedicated per-app one. Satisfied by apps.Service; kept as an
// interface here for the same reason as ProcSource, so the metrics package
// doesn't import apps.
type DBSource interface {
	DBContainers() []string
}
