# Acknowledgements

I took a lot of inspiration from the following repos:
- https://github.com/get-eventually/go-eventually - many thanks to this one
- https://github.com/eugene-khyst/postgresql-event-sourcing
- https://github.com/hallgren/eventsourcing
	- https://github.com/hallgren/wtf
- https://github.com/thefabric-io/eventsourcing
	- https://github.com/thefabric-io/eventsourcing.example

Other resources for my event sourcing journey in no particular order:
- https://threedots.tech
- [Every System is a Log: Avoiding coordination in distributed applications](https://news.ycombinator.com/item?id=42813049)
- Implementing Domain-driven Design by Vaughn Vernon
- https://khalilstemmler.com/articles/categories/domain-driven-design/
- https://blog.devgenius.io/go-golang-clean-architecture-repositories-vs-transactions-9b3b7c953463
- [Martin Fowler - Modularizing react apps](https://martinfowler.com/articles/modularizing-react-apps.html) - yes I also write a lot of react typescript
- https://refactoring.com/catalog/replaceConditionalWithPolymorphism.html
- https://watermill.io/advanced/forwarder/
- https://github.com/Sairyss/domain-driven-hexagon
- https://martendb.io/introduction.html
- https://getakka.net/articles/persistence/event-sourcing.html
- https://github.com/oskardudycz/EventSourcing.NetCore
	- https://domaincentric.net/blog/event-sourcing-projections
	- https://event-driven.io/en/dealing_with_eventual_consistency_and_idempotency_in_mongodb_projections
		- https://event-driven.io/en/projections_and_read_models_in_event_driven_architecture/
- https://github.com/AxonFramework/AxonFramework
- https://occurrent.org/
- https://dewdrop.events/
- https://docs.kurrent.io/getting-started/features.html
	- https://docs.kurrent.io/getting-started/concepts.html#event-stream
	- https://docs.kurrent.io/server/v25.0/features/projections/
- https://github.com/eugene-khyst/postgresql-event-sourcing
- https://zitadel.com/docs/concepts/architecture/software
	- https://zitadel.com/docs/concepts/eventstore/implementation
- https://skoredin.pro/blog/golang/event-sourcing-go

I found that none of them were as flexible as I'd like - a lot of them were only tied to specific storage (like postgres) or were **very** cumbersome to read (talking mostly about the java/dotnet ones here).
