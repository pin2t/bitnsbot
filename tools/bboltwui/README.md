# bboltwui

A small web UI for a [bbolt](https://github.com/etcd-io/bbolt) database. Point it
at a file and browse, edit, delete, export or import any bucket from a browser.

It is the same interface the bot serves under `-dbui-listen` (the `dbui`
package), running as its own process — so a database can be inspected without
the bot running, or on a copy taken by `-backup`.

## Running

```
go build -o bboltwui ./tools/bboltwui/
./bboltwui -db bitnsbot.db -listen 127.0.0.1:8090
```

Then open <http://127.0.0.1:8090>. Both flags are required:

| flag      | what it is                                                |
|-----------|-----------------------------------------------------------|
| `-db`     | path to the bbolt file                                    |
| `-listen` | address to serve on, e.g. `127.0.0.1:8090`                 |

**Bind it to localhost.** There is no authentication and the UI can write and
delete any bucket, so it must never be exposed to a network.

bbolt locks the file exclusively, so it cannot open a database another process
already has open. It **waits for the lock rather than failing**, so against a
running bot it simply hangs with nothing printed — no error, and no `database UI
listening` line. That silence is the symptom: stop the bot first, or point it at
a copy (`-backup` takes consistent ones while the bot runs).

## What it looks like

Pick a bucket, press **View**, and the rows come back a page at a time. Keys and
values that are not text — big-endian heights, the packed address index — are
shown behind a `hex:` marker, which is also how you type one back in.

![Browsing a bucket](screenshot-data.png)

Clicking a row opens its value in the **Value** tab, where **Put** writes it
back. **Get** does the same for a key typed in directly, and the trash icon on a
row deletes that key straight away — no confirmation, since clearing a bucket a
dialog at a time would be unusable.

![Editing a value](screenshot-value.png)

The page is a single embedded HTML file that fetches nothing external, and the
whole UI is eight JSON endpoints: `/api/buckets`, `/api/view`, `/api/get`,
`/api/put`, `/api/delete`, `/api/clearbucket`, `/api/export` and `/api/import`.
