# 📸 Detailed Immich Optimization Pipeline Architecture

This document outlines the three-stage architecture for a self-hosted media backup and optimization system. This approach prioritizes **storage efficiency** and **data resilience** by decoupling transfer, processing, and cataloging.

---

## I. The System Components and Roles

| Component | Primary Role | Key Mechanism |
| :--- | :--- | :--- |
| **Mobile Client (FolderSync)** | **Secure Ingestion & Deduplication** | Manages one-way sync, retries, battery optimization, and network constraints (Wi-Fi/VPN). |
| **Network Server (SFTP/SMB/FTP)** | **Landing Zone Repository** | Provides the network service (SFTP, SMB, etc.) for secure file delivery to the **watched folder**. |
| **Immich Optimizer** | **Transactional Processing Engine** | Compresses original files, uploads the smaller version to Immich, and handles the atomic cleanup/failover. |
| **Immich** | **Final Catalog & Indexing** | Stores the *optimized* file, generates thumbnails, extracts metadata, and provides the user interface. |

---

## II. Stage 1: Secure Ingestion and Atomic Placement

This stage moves the original media from the mobile device to a secured location on the server's local disk, creating the initial **safety buffer**.

### 1. Transfer Protocol Selection and Setup

| Protocol | Security Note | Use Case & Server Setup Example |
| :--- | :--- | :--- |
| **SFTP** | **Encrypted** (via SSH). **Recommended.** | Best for transfers across the internet or untrusted networks. Uses Docker image like `atmoz/sftp`. |
| **SMB** | **Encrypted** (if v3+ is used). | Excellent performance for **Local Area Network (LAN) only** setups. Requires a host-based Samba service. |
| **FTP** | **Plaintext.** **Discouraged.** | Only acceptable on highly trusted, isolated internal networks due to sending credentials unencrypted. |

### 2. Client-Side Transfer Logic

1.  **Client Connection:** The **FolderSync** app initiates a connection using the chosen protocol (SFTP, SMB, etc.) and authenticates.
2.  **Deduplication Check:** FolderSync runs its **strict one-way sync** check, ensuring it only selects files not previously marked as successfully transferred.
3.  **Encrypted Data Transfer:** The original, high-resolution media file is transferred across the network to the server.
4.  **File Landing:** The network server container (e.g., SFTP) uses a **volume/bind mount** to write the original file directly into the **watched folder** on the host filesystem. The media is now secured on the server.

---

## III. Stage 2: Atomic Optimization and API Upload

The **Immich Optimizer** container monitors the mounted **watched folder** and executes a transactional loop to process the files.

### 1. Processing Loop and Atomicity

1.  **Polling and Detection:** The Optimizer runs a continuous loop, identifying a new, unprocessed **Original File** in the watched directory.
2.  **File Read & Optimization:** The Original File is read, and the compression utility is executed (e.g., converting a large JPEG to a smaller JPEG-XL or transcoding a video).
3.  **New File Creation:** A new **Optimized File** is created. This step is critical because the act of compression generates a file with a completely different **digital hash**.
4.  **Immich API Upload:** The Optimizer sends the smaller **Optimized File** to the Immich API endpoint.
5.  **Confirmation & Clean Up:** The Optimizer waits for an explicit success confirmation (HTTP `200 OK`) from the Immich API, verifying the file has been accepted and written to Immich's storage. Only then is the large **Original File deleted** from the watched folder.

### 2. Robust Failover Logic

* **Success:** Original file is deleted.
* **Failure:** If the optimization or upload fails for any reason (e.g., corrupt file, network interruption, Immich server downtime), the Original File is saved to the **undone/** folder as `undone/<sha1>/original.file`, next to a `metadata.json` that says why. This ensures **zero data loss** and reserves the photo for manual troubleshooting. The original is never uploaded in place of a failed conversion. Identical content is never stored twice (each new failure is appended to its `metadata.json`), and a different file with the same name gets its own folder, so it never overwrites an earlier one.
* **Shutdown:** a file whose processing is interrupted by a restart stays in the watched folder and is processed again at the next start.

---

## III-b. The Conversion Engine (shared by every source)

The watcher is one **source** of files. The reverse proxy (`IO_MODE=proxy`) is another, and a dedicated mobile/desktop app can be added later the same way. All sources hand files to one **conversion engine** (`internal/usecase/convert`, port `port.Converter`):

```
watcher ─┐                                ┌─> deliver result (upload to Immich)
proxy   ─┼─> Convert(ctx, input, meta) ───┤
app     ─┘   (task selection, global      └─> failure: original saved to undone/
              concurrency limit, timeout)      + metadata.json, error returned
```

* **Task selection and the decision** between the processed output (smaller) and the original (no gain) happen once, in the engine.
* **One concurrency limit** (`IO_MAX_CONCURRENT_REQUESTS`) covers conversions from all sources together.
* **The undone folder belongs to the engine.** Whenever conversion fails, or no task matches (unless `IO_UNDONE_UNMATCHED=false`), the engine saves the original input to `IO_UNDONE_DIR` as `undone/<sha1-of-content>/original.file` plus `metadata.json`, then returns an error to the source. The content identity is SHA-1 because that is the checksum Immich stores for each asset (hex in the folder name, base64 too in `metadata.json`), so undone files can be matched against Immich; SHA-1 is used for identity, not security. Folders from metadata version 2 (named after SHA-256) are a different identity and are never touched. The proxy's coalescing key also uses the file's SHA-1; its form and caller fingerprints are internal SHA-256 digests. The layout is flat and content-addressed: no per-source folders and no mirrored directory tree; source, original filename and original directory (watcher: relative to the watch root; proxy: the folder part of the client's multipart filename, if any) are metadata only. Folders are assembled under a temp name and renamed into place, so a crash never leaves a complete-looking half folder. Identical content is never copied twice: the hash is known (proxy) or computed first (watcher), and an existing folder only gets its `metadata.json` updated (atomically, fsynced). Processes sharing one undone dir serialise folder publishing and metadata updates with a `flock` on `undone/.io-undone.lock`. A source only builds the input, calls `Convert`, and delivers the result.
* **Source differences:**
  * *Watcher:* deletes the source file once it is uploaded or saved to undone. Upload failures (e.g. quota exceeded) are also saved to undone, because the watched folder is the only other copy.
  * *Proxy:* returns an error to the client (`415` / `502`), and the client keeps its original. Upload failures to Immich are **not** saved to undone, since the client retries them. Jobs run detached from the client connection, so a disconnect or client timeout does not abort conversion or upload, and identical retries are coalesced onto the same job. The coalescing key covers the file content, a digest of every other form field (so e.g. two uploads differing only in `livePhotoVideoId` are separate), the method, path and query, and a hash of the caller's credentials (`Authorization`, `x-api-key`, session cookie, ...), which are never logged. Proxy-generated error bodies never contain the text `Quota has been exceeded!`, because the Immich mobile app aborts the whole backup run on it.
* **Cancellation is not failure:** on shutdown, running jobs are given `IO_SHUTDOWN_TIMEOUT_SECONDS` to finish and are then cancelled (killing the task's whole process group). Nothing is saved to undone for a cancelled job.

---

## IV. Stage 3: Final Storage

**Permanent Storage:** The final, optimized version of the file is written to Immich, ready for access via all client applications.

---

## ✅ Architectural Resilience (Why this Setup is Best)

This pipeline's complexity is intentional, providing features essential for reliable self-hosting:

* **Client Resilience:** FolderSync handles automatic retries, respects connectivity rules (Wi-Fi/VPN), and bypasses mobile operating system limitations (like Android battery optimization) that often kill background uploads.
* **Server Safety Buffer:** The **watched folder** acts as a reliable safety buffer. If the **Immich** or **Optimizer** services are temporarily down for maintenance or updates, the mobile client still successfully offloads the files to the server's disk via SFTP/SMB. Your photos are **secured immediately** on the server's permanent storage, minimizing the risk of data loss compared to the official Immich app, which requires the entire server stack to be online for backup.
