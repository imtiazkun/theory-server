# theory-server

theory-server is the backend that lets a team of engineers hold one theoretical model together. It hosts the vaults as git repositories, checks every push against the model's rules, and holds the shared compile lock, marker and reports.

Stack: golang

## Running it

    THEORY_DOMAIN=theory.example.com docker compose up -d

Caddy serves HTTPS in front of the server. All state lives in the `theory-data` volume, mounted at `/data`. Copying it while the server is stopped is a full backup.

## Admin commands

Run them on the server, for example with `docker compose exec theory-server theory-server ...`:

    theory-server vault create <name>
    theory-server member add --name "<name>" --email <email>
    theory-server member remove --email <email>
    theory-server token create --email <email>
    theory-server token revoke --email <email>
    theory-server lock break <vault>

A vault's URL is `https://<server>/git/<name>.git`. Git takes the token as the password; the user name is ignored.

## API

Under `/api/v1`, with `Authorization: Bearer <token>`:

| Request | Answer |
| --- | --- |
| `GET /api/v1/me` | `{name, email}` |
| `GET /api/v1/vaults/{vault}/lock` | `{holder: {name, email}, since}`, or `{holder: null}` |
| `POST /api/v1/vaults/{vault}/lock` | `{lockId, expiresAt}`, or 409 with `{error, holder, since}` |
| `POST /api/v1/vaults/{vault}/lock/{lockId}/renew` | `{expiresAt}`, or 410 |
| `POST /api/v1/vaults/{vault}/lock/{lockId}/release` with `{marker?, report?}` | `{marker}`, or 410 |
| `GET /api/v1/vaults/{vault}/reports?before={reportId}` | `{reports: [{id, member, createdAt, report}]}`, newest first, 20 at a time |

See AGENTS.md for tests and running on a laptop.
