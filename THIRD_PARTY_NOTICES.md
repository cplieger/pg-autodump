# Third-party notices

No third-party code is included in this repository.

One design is followed without any of its code being included, and it is named at the line that follows it:

- The `pg_dump` argv construction and exit-code handling in `internal/pg/pg.go` follow [orgrim/pg_back](https://github.com/orgrim/pg_back) (BSD-2-Clause), used as a reference only. The dial-then-`psql` probe that separates a connection failure from an authentication failure is this project's own.
