# Event transformers

You can find this example in [./examples/4_transformers_crypto/main.go](../examples/4_transformers_crypto/main.go) and [account_aes_crypto_transformer.go](../examples/internal/account/account_aes_crypto_transformer.go).

Transformers allow you to modify events just before they are encoded and written to the event log, and right after they are read and decoded. This is a powerful hook for implementing cross-cutting concerns.

Common use cases include:

- **Encryption**: Securing sensitive data within your events.
- **Compression**: Reducing the storage size of large event payloads.
- **Up-casting**: Upgrading older versions of an event to a newer schema on the fly.

When you provide multiple transformers, they are applied in the order you list them for writing, and in the **reverse order** for reading. For example: `encrypt -> compress` on write becomes `decompress -> decrypt` on read.

## Further reading

- [Example: Crypto shedding for GDPR](crypto-shedding-gdpr.md)
- [Global Transformers with `AnyTransformerToTyped`](global-transformers.md)
- [Event versioning and upcasting](event-versioning-upcasting.md)
