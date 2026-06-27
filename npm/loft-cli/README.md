# loft-cli

Deploy apps to a [Loft](https://github.com/larsakerlund/loft) platform.

```bash
npx loft-cli login https://loft.example.com   # discovers OAuth config from the URL, device-flow sign-in
npx loft-cli deploy ./dist --name blog        # upload a folder; serves at https://blog.<your-domain>
npx loft-cli delete blog
```

`login <url>` reads the platform's `/.well-known/loft` (over HTTPS) and signs you in via the OAuth
device flow, saving the token. `deploy` validates the folder locally, then uploads it. For CI, set
`LOFT_TOKEN` to a bearer token instead of running `login`; the platform URL comes from `--url`,
`LOFT_URL`, or the saved login.

The CLI is a single Go binary. This package installs the prebuilt binary for your platform via an
optional dependency (no build step, no postinstall download).
