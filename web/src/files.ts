// Turning a browser drop (or file picker) into a flat list of files with site-relative paths.
// A drop can contain folders, so we walk the FileSystemEntry tree; the picker gives flat files.
// The path is what the site is served at, so "assets/app.js" stays "assets/app.js", and a single
// dropped folder is unwrapped (its name is the wrapper, not part of the URL).

export interface DeployFile {
  /** Site-relative path, forward slashes, no leading slash. */
  path: string;
  file: File;
}

interface FileSystemEntryLike {
  isFile: boolean;
  isDirectory: boolean;
  fullPath: string;
}
interface FileSystemFileEntry extends FileSystemEntryLike {
  file(onSuccess: (file: File) => void, onError: (err: unknown) => void): void;
}
interface FileSystemDirectoryEntry extends FileSystemEntryLike {
  createReader(): {
    readEntries(onSuccess: (entries: FileSystemEntryLike[]) => void, onError: (err: unknown) => void): void;
  };
}

function readEntry(entry: FileSystemFileEntry): Promise<File> {
  return new Promise((resolve, reject) => {
    entry.file(resolve, reject);
  });
}

function readDir(dir: FileSystemDirectoryEntry): Promise<FileSystemEntryLike[]> {
  const reader = dir.createReader();
  const all: FileSystemEntryLike[] = [];
  // readEntries returns in batches; keep calling until it yields an empty batch.
  return new Promise((resolve, reject) => {
    const pump = (): void => {
      reader.readEntries((batch) => {
        if (batch.length === 0) {
          resolve(all);
          return;
        }
        all.push(...batch);
        pump();
      }, reject);
    };
    pump();
  });
}

async function walk(entry: FileSystemEntryLike, out: DeployFile[]): Promise<void> {
  if (entry.isFile) {
    const file = await readEntry(entry as FileSystemFileEntry);
    out.push({ path: entry.fullPath.replace(/^\/+/, ""), file });
    return;
  }
  if (entry.isDirectory) {
    const children = await readDir(entry as FileSystemDirectoryEntry);
    await Promise.all(children.map((child) => walk(child, out)));
  }
}

/** Strip a single shared top-level folder, so dropping a `site/` folder serves at the root. */
function unwrapRoot(files: DeployFile[]): DeployFile[] {
  if (files.length === 0) return files;
  const firstPath = files[0]?.path ?? "";
  const top = firstPath.split("/")[0] ?? "";
  if (!top) return files;
  const allShareTop = files.every((f) => f.path === top || f.path.startsWith(`${top}/`));
  const anyNested = files.some((f) => f.path.includes("/"));
  if (!allShareTop || !anyNested) return files;
  return files.map((f) => ({ ...f, path: f.path.slice(top.length + 1) })).filter((f) => f.path !== "");
}

/** Collect files from a drop's DataTransfer, walking any dropped folders. */
export async function filesFromDrop(dt: DataTransfer): Promise<DeployFile[]> {
  const entries: FileSystemEntryLike[] = [];
  for (const item of Array.from(dt.items)) {
    const getAsEntry = (item as DataTransferItem & { webkitGetAsEntry?: () => FileSystemEntryLike | null })
      .webkitGetAsEntry;
    const entry = getAsEntry?.call(item) ?? null;
    if (entry) entries.push(entry);
  }
  if (entries.length === 0) {
    // Fallback for browsers without the entries API: flat files only.
    return Array.from(dt.files).map((file) => ({ path: file.name, file }));
  }
  const out: DeployFile[] = [];
  await Promise.all(entries.map((entry) => walk(entry, out)));
  return unwrapRoot(out);
}

/** Collect files from an <input type="file"> picker (webkitdirectory keeps relative paths). */
export function filesFromInput(list: FileList): DeployFile[] {
  const files = Array.from(list).map((file) => ({
    path: (file.webkitRelativePath || file.name).replace(/^\/+/, ""),
    file,
  }));
  return unwrapRoot(files);
}
