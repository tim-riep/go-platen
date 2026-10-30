# platen

platen is a print management server with distributed print workers, written in Go.

Applications (initially a web ERP) submit print jobs over HTTP. The server holds
the printer registry, queues and jobs. Workers located near the printers fetch
jobs, render them and send them to the printers. All traffic between the
components runs over an embedded [NetBird](https://netbird.io) client; platen
opens no port on any host network interface.

> Status: under development.

## Architecture

_To be written._

## NetBird setup

_To be written._

## Configuration

_To be written._

## API overview

_To be written._

## License

platen is licensed under the [Apache License 2.0](LICENSE). Third-party
dependencies and their licenses are listed in
[THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES).
