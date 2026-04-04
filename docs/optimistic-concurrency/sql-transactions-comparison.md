# How is this different from SQL transactions?

SQL transactions often use **pessimistic locking** - which means you assume a conflict is likely and you lock the data upfront to prevent it:

1. **`BEGIN TRANSACTION;`**: Start a database transaction.
2. **`SELECT balance FROM accounts WHERE id = 'acc-123' FOR UPDATE;`**: This is the critical step. The `FOR UPDATE` clause tells the database to place a **write lock** on the selected row.
3. While this lock is held, no other transaction can read that row with `FOR UPDATE` or write to it. Any other process trying to do so will be **blocked** and forced to wait until the first transaction is finished.
4. The application code checks if `balance >= amount_to_withdraw`.
5. **`UPDATE accounts SET balance = balance - 50 WHERE id = 'acc-123';`**: The balance is updated.
6. **`COMMIT;`**: The transaction is committed, and the lock on the row is released.

In this model, a race condition like the one in our example is impossible. User B's transaction would simply pause at step 2, waiting for User A's transaction to `COMMIT` or `ROLLBACK`. 

The database itself serializes the access. 

In **optimistic concurrency control** - we shift the responsability from out database to our application.
