# Loft

Loft gives every static site a backend. You write a frontend, deploy the folder, and the JavaScript
in it reaches a same-origin API for data, files, realtime, and AI, with no server to build or run.
One Loft serves many isolated sites. It is an open-source, self-hosted take on the idea behind
Shopify's [Quick](https://shopify.engineering/quick).

## Deploy a site

Install the CLI and point it at Loft:

```bash
npm install -g loft-cli

loft login https://loft.example.com
loft deploy ./dist blog                # live at https://blog.loft.example.com
loft delete blog
```

`loft deploy` checks the folder locally and uploads it; the site is live at a subdomain of the
platform. Or skip the CLI entirely: open the platform in a browser and drag a folder onto the page to
deploy it. No Loft to deploy to yet? See [Run your own](#run-your-own).

## Build the app

A deployed site bundles [`loft-js`](https://github.com/larsakerlund/loft-js), a small browser SDK.
Every call is same-origin, scoped to the site and the signed-in user:

```js
import loft from "loft-js";

const me = await loft.user.me();                    // the signed-in user
const posts = loft.db.collection("posts");          // a per-site document store
await posts.create({ title: "Hello" });
const { url } = await loft.upload(file);             // upload a file, get a URL back
loft.socket.channel("lobby").on(render);            // realtime between visitors
const reply = await loft.ai.chat([{ role: "user", content: "..." }]); // an LLM, no key in the client
```

The keys, database, and storage stay server-side, and each site's data is isolated from the rest. The
full API reference lives in the [`loft-js`](https://github.com/larsakerlund/loft-js) repo.

## Run your own

Loft is self-hosted. To see it end to end on your machine:

```bash
docker compose up --build
# open http://localhost:8088          drop a folder to deploy
# a deployed site is then at http://<name>.localhost:8088
```

A production deployment runs behind your own authenticating proxy and a few backing services. See
[docs/deploying.md](docs/deploying.md) for the setup and [ARCHITECTURE.md](ARCHITECTURE.md) for how
the pieces fit and how sites stay isolated.

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) to get started. Licensed under
[MIT](LICENSE).
