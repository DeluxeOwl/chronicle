# Core Packages

These packages define the core abstractions and contracts of the framework.

* `chronicle`: The top level package re-exports the repositories and event registries for ease of use.
* `aggregate/`: Contains the building blocks for your domain models, like `aggregate.Base` and `aggregate.Root`. It also provides the main `Repository` interfaces for loading and saving your aggregates.
* `event/`: This package defines the contracts (interfaces) for fundamental concepts. It includes `event.Any` (the base for all events), `event.Log` (the interface for any event store), and `event.Registry`.
* `version/`: This package defines `version.Version` and the `version.ConflictError` that is returned on write conflicts.
* `encoding/`: This package provides the interface and default JSON implementation for encoding and decoding your event structs into a storable format.
* `examples/`: This package contains the examples from this README.
