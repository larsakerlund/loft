// The root deploy page: drop files, name a subdomain, launch, watch it go live. The whole page is the
// drop target. State is a small machine: editing (collect files + name) -> deploying -> live | error,
// with a confirm step (a Base UI popover anchored to the launch button) when the site already exists.

import { useCallback, useRef, useState } from "react";
import { Popover } from "@base-ui/react/popover";
import confetti from "canvas-confetti";
import { filesFromDrop, filesFromInput, type DeployFile } from "./files";
import { deploy, siteSuffix, SiteExistsError } from "./deploy";
import logoUrl from "./assets/loft-logo.png";

type Phase = "editing" | "deploying" | "confirm" | "live" | "error";

// Subdomain = one DNS label: lowercase, alphanumeric and hyphens, no leading/trailing or doubled
// hyphen, capped at the 63-char DNS limit. Sanitized as the user types so the value is always valid.
const MAX_SITE = 63;
function sanitizeSite(input: string): string {
  return input
    .toLowerCase()
    .replace(/[^a-z0-9-]/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-/, "")
    .slice(0, MAX_SITE);
}

export function App(): React.JSX.Element {
  const [files, setFiles] = useState<DeployFile[]>([]);
  const [site, setSite] = useState<string>("");
  const [phase, setPhase] = useState<Phase>("editing");
  const [error, setError] = useState<string>("");
  const [liveUrl, setLiveUrl] = useState<string>("");
  const [dragging, setDragging] = useState<boolean>(false);
  const dragDepth = useRef(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const launchRef = useRef<HTMLButtonElement>(null);

  const addFiles = useCallback((next: DeployFile[]) => {
    if (next.length === 0) return;
    setFiles((prev) => {
      const byPath = new Map(prev.map((f) => [f.path, f]));
      for (const f of next) byPath.set(f.path, f);
      return [...byPath.values()];
    });
    setPhase("editing");
    setError("");
  }, []);

  const onDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault();
      dragDepth.current = 0;
      setDragging(false);
      if (phase === "deploying") return;
      void filesFromDrop(e.dataTransfer).then(addFiles);
    },
    [addFiles, phase],
  );

  const launch = useCallback(
    (overwrite: boolean) => {
      const name = site.replace(/-$/, ""); // a trailing hyphen is fine mid-edit, not on submit
      if (files.length === 0 || !name) return;
      setPhase("deploying");
      setError("");
      deploy(name, files, overwrite)
        .then((res) => {
          setLiveUrl(res.url);
          setFiles([]); // clear so the next drop starts a fresh deploy, not a merge into this one
          setPhase("live");
          void confetti({ particleCount: 160, spread: 75, origin: { y: 0.3 } });
        })
        .catch((err: unknown) => {
          if (err instanceof SiteExistsError) {
            setPhase("confirm"); // ask before clobbering an existing site
            return;
          }
          setError(err instanceof Error ? err.message : "deploy failed");
          setPhase("error");
        });
    },
    [files, site],
  );

  const reset = useCallback(() => {
    setFiles([]);
    setSite("");
    setLiveUrl("");
    setError("");
    setPhase("editing");
  }, []);

  return (
    <div
      className={`grid-bg flex min-h-screen flex-col items-center px-5 pt-16 pb-16 text-ink transition-colors ${dragging ? "bg-canvas-drag" : "bg-canvas"}`}
      onDragEnter={(e) => {
        e.preventDefault();
        dragDepth.current += 1;
        setDragging(true);
      }}
      onDragOver={(e) => {
        e.preventDefault();
      }}
      onDragLeave={(e) => {
        e.preventDefault();
        dragDepth.current -= 1;
        if (dragDepth.current <= 0) setDragging(false);
      }}
      onDrop={onDrop}
    >
      <header className="mt-2 mb-10 flex w-full max-w-[560px] justify-center">
        <img src={logoUrl} alt="Loft" className="h-24 w-auto select-none" draggable={false} />
      </header>

      {phase === "live" ? (
        <LiveCard url={liveUrl} onAgain={reset} />
      ) : (
        <main className="flex w-full max-w-[560px] flex-col gap-4">
          <Dropzone count={files.length} dragging={dragging} onPick={() => inputRef.current?.click()} />
          <input
            ref={inputRef}
            type="file"
            multiple
            // @ts-expect-error non-standard but widely supported: lets users pick a whole folder.
            webkitdirectory=""
            className="hidden"
            onChange={(e) => {
              if (e.target.files) addFiles(filesFromInput(e.target.files));
              e.target.value = "";
            }}
          />

          {files.length > 0 ? (
            <>
              <FileList files={files} />
              <LaunchBar
                site={site}
                suffix={siteSuffix()}
                busy={phase === "deploying"}
                confirming={phase === "confirm"}
                buttonRef={launchRef}
                onSite={(v) => {
                  setSite(sanitizeSite(v));
                  // editing the name retires the conflict, so close the prompt and let them launch
                  if (phase === "confirm") setPhase("editing");
                }}
                onLaunch={() => {
                  launch(false);
                }}
              />
              <Popover.Root
                open={phase === "confirm"}
                onOpenChange={(open) => {
                  if (!open) setPhase("editing"); // outside-click / Escape dismisses to editing
                }}
              >
                <Popover.Portal>
                  <Popover.Positioner anchor={launchRef} side="bottom" align="end" sideOffset={12}>
                    <Popover.Popup className="card-shadow animate-pop w-[min(340px,calc(100vw-2rem))] rounded-xl border border-line bg-white p-4 outline-none">
                      <Popover.Arrow className="size-3 rotate-45 bg-white border-line data-[side=bottom]:-top-1.5 data-[side=bottom]:border-t data-[side=bottom]:border-l data-[side=top]:-bottom-1.5 data-[side=top]:border-r data-[side=top]:border-b" />
                      <p className="mb-3 text-sm">
                        <strong>{site}</strong> already exists. Overwrite it?
                      </p>
                      <div className="flex justify-end gap-2">
                        <button
                          type="button"
                          className="cursor-pointer rounded-[10px] bg-[#eef0f3] px-4 py-2 font-semibold text-ink"
                          onClick={() => {
                            setPhase("editing");
                          }}
                        >
                          Cancel
                        </button>
                        <button
                          type="button"
                          className="cursor-pointer rounded-[10px] bg-overwrite px-4 py-2 font-semibold text-white"
                          onClick={() => {
                            launch(true);
                          }}
                        >
                          Overwrite
                        </button>
                      </div>
                    </Popover.Popup>
                  </Popover.Positioner>
                </Popover.Portal>
              </Popover.Root>
            </>
          ) : null}

          {phase === "error" ? <p className="text-center text-sm text-[#b91c1c]">{error}</p> : null}
        </main>
      )}
    </div>
  );
}

function Dropzone(props: { count: number; dragging: boolean; onPick: () => void }): React.JSX.Element {
  const { count, dragging, onPick } = props;
  return (
    <button
      type="button"
      className={`card-shadow flex cursor-pointer flex-col items-center gap-2 rounded-2xl border-2 border-dashed bg-white px-6 py-12 transition ${dragging ? "scale-[1.01] border-accent" : "border-line"}`}
      onClick={onPick}
    >
      <span className="mb-3 grid size-14 place-items-center rounded-[14px] bg-accent text-2xl font-bold text-white" aria-hidden>
        ↑
      </span>
      {count > 0 ? (
        <span className="text-xl font-bold tracking-tight">
          {count} file{count === 1 ? "" : "s"} ready to deploy
        </span>
      ) : (
        <>
          <span className="text-xl font-bold tracking-tight">Drop your files here</span>
          <span className="text-sm text-muted">HTML · JS · CSS · or a whole folder</span>
        </>
      )}
    </button>
  );
}

// Render at most a screenful of rows; a big deploy (nested folders) shouldn't become an endless
// scroll, and the list is informational (there's no per-file action). The rest collapse to a count.
const MAX_LISTED = 100;

function FileList(props: { files: DeployFile[] }): React.JSX.Element {
  const shown = props.files.slice(0, MAX_LISTED);
  const extra = props.files.length - shown.length;
  return (
    <ul className="card-shadow m-0 max-h-[230px] list-none overflow-auto rounded-2xl border border-line bg-white p-2">
      {shown.map((f) => (
        <li
          key={f.path}
          className="flex justify-between gap-4 rounded-lg px-2.5 py-[0.45rem] font-mono text-sm odd:bg-[#fafbfc]"
        >
          <span className="overflow-hidden text-ellipsis whitespace-nowrap">{f.path}</span>
          <span className="shrink-0 text-muted">{formatSize(f.file.size)}</span>
        </li>
      ))}
      {extra > 0 ? (
        <li className="px-2.5 py-[0.45rem] text-center font-mono text-sm text-muted">
          + {extra} more file{extra === 1 ? "" : "s"}
        </li>
      ) : null}
    </ul>
  );
}

function LaunchBar(props: {
  site: string;
  suffix: string;
  busy: boolean;
  confirming: boolean;
  buttonRef: React.RefObject<HTMLButtonElement | null>;
  onSite: (v: string) => void;
  onLaunch: () => void;
}): React.JSX.Element {
  const { site, suffix, busy, confirming, buttonRef, onSite, onLaunch } = props;
  const ready = !busy && !confirming && Boolean(site);
  return (
    <form
      className="flex gap-3"
      onSubmit={(e) => {
        e.preventDefault(); // Enter in the field submits here instead of reloading the page
        if (ready) onLaunch();
      }}
    >
      <label className="card-shadow flex min-w-0 flex-1 items-center rounded-xl border border-line bg-white px-[0.9rem]">
        <input
          className="min-w-0 flex-1 border-0 bg-transparent py-[0.85rem] font-semibold outline-none"
          value={site}
          placeholder="your-site"
          spellCheck={false}
          autoCapitalize="off"
          aria-label="subdomain"
          onChange={(e) => {
            onSite(e.target.value);
          }}
        />
        <span className="font-mono text-sm whitespace-nowrap text-muted">{suffix}</span>
      </label>
      <button
        ref={buttonRef}
        type="submit"
        className="card-shadow min-w-40 cursor-pointer rounded-xl border-0 bg-go px-6 text-center font-bold text-white transition hover:brightness-105 active:translate-y-px disabled:cursor-default disabled:opacity-50"
        disabled={!ready}
      >
        {busy || confirming ? "Launching…" : "Launch!"}
      </button>
    </form>
  );
}

function LiveCard(props: { url: string; onAgain: () => void }): React.JSX.Element {
  return (
    <main className="mt-8 flex w-full max-w-[560px] flex-col items-center gap-4 text-center">
      <h1 className="m-0 text-2xl font-bold">Your site is live!</h1>
      <a
        className="card-shadow rounded-xl border border-line bg-white px-5 py-[0.85rem] font-mono text-base text-accent"
        href={props.url}
        target="_blank"
        rel="noreferrer"
      >
        {props.url}
      </a>
      <button type="button" className="mt-2 cursor-pointer border-0 bg-transparent text-muted underline" onClick={props.onAgain}>
        Deploy another
      </button>
    </main>
  );
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${String(bytes)} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
