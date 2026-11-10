# internal/webtools

Native search and page-reader adapter. It maps one vendor (Tavily) onto the two
HTTP contracts Kimi's own kernel calls, and serves them from a private loopback
listener.

```text
WebSearch  POST <base_url>  {text_query}  Authorization: Bearer <api_key>
           -> 200 {"search_results":[{"title","url","snippet","date?","site_name?"}]}
FetchURL   POST <base_url>  {url}         Authorization: Bearer <api_key>
           -> 200 text/markdown
```

## Wiring

```go
// The caller owns where the vendor key lives.
type keyStore struct{ path string }
func (k keyStore) Get() (string, error)  { /* return "" when unset */ }
func (k keyStore) Set(v string) error    { /* store it however you like */ }

service := webtools.NewService(webtools.ServiceOptions{
    Settings:    webtools.FileSettings{Path: filepath.Join(stateDir, "web-tools-settings.json")},
    Credentials: keyStore{path: credPath},
    AuthFile:    filepath.Join(stateDir, "web-tools-local-auth.json"),
})
defer service.Close()

// GET /api/web-tools
state := service.State(nativeStatus)          // no request is made

// POST /api/web-tools  {action:"save", provider:"tavily", apiKey:"..."}
block, err := service.SaveProvider("tavily", apiKey)   // starts the listener
if err != nil { /* *webtools.Error carries Code and Status */ }
if err := service.Commit("tavily"); err != nil { ... }

// The block Kimi's config needs:
//   block["search"] = {enabled, baseURL: "http://127.0.0.1:<port>/search", apiKey: <64-hex>}
//   block["fetch"]  = {enabled, baseURL: "http://127.0.0.1:<port>/fetch",  apiKey: <64-hex>}

// POST /api/web-tools  {action:"probe", provider:"tavily"}
answer := service.Probe("tavily", "")         // {search:{state,count}, fetch:{state,pageChars}}
```

`State`, `SaveProvider` and `Probe` never send a request on their own except
`Probe`, which sends the fixed sample only.

## Two facts the panel must keep apart

A passing probe means the account and endpoint answer in the native shape. It
says nothing about whether a running Kimi session has reloaded its endpoint.
`State` reports `pendingRestart` for exactly that reason, and the probe message
says so.

## What it refuses

- a vendor endpoint that is not plain HTTPS (loopback may be plain HTTP)
- an endpoint carrying credentials, a query or a fragment
- a vendor key that is not printable ASCII without surrounding space
- a search query over 2000 characters, or a fetch URL that is not http(s)
- a redirect (the key is never replayed at another host)
- an answer over 4 MiB (search) or 8 MiB (fetch)
- any inbound request whose Host, Origin, `Sec-Fetch-Site` or bearer token is not
  the local one, and any body over 64 KiB

## Extraction failures are not empty pages

A 200 from the extract endpoint is not a success on its own. A URL can land in
`failed_results`, an entry can carry empty `raw_content`, and a single result is
only accepted when the vendor normalised the URL. Each of those is a
`web_adapter_extract_failed` so the kernel shows an error instead of a blank
page.
