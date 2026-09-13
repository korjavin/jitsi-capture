# jitsi2outline

_This document is the original design spec. The product has since been split into three
services (jitsi-capture → transcriber → tr2outline), and this repository is jitsi-capture
only — see `CLAUDE.md` for the current architecture. A full rewrite of this README is
tracked separately._

A self-hosted service for private, local recording of **Jitsi Meet** calls, transcription on **CPU**, and saving the results into the **Outline** knowledge base, with **Zulip** integration.

---

## 🎯 Project goals and context

1. **Call context:** Jitsi video calls are started from the **Zulip** messenger (by clicking the call button, which generates a link to a `meet.jit.si/<room>` room).
2. **On-demand joining:** The bot should join a call not permanently, but on request from the participants (via a command/notification from Zulip or a webhook).
3. **100% on-premise / privacy:** Audio data must not leave for third-party clouds (Recall.ai, Otter, Fireflies, etc.). All processing happens strictly inside the local perimeter.
4. **Transcription on CPU:** Audio processing runs locally on CPU (speed is not critical; the priority is quality and reliability).
5. **Outline integration:** Transcription results are formatted as Markdown and published automatically into the **Outline** knowledge base via its REST API.
6. **Feedback into Zulip:** A link to the finished document in Outline is sent back to the same stream/topic in Zulip.

---

## 🏗️ System architecture

```text
               1. @transcribe start / link
  [ Zulip ] ───────────────────────────────────► [ Bot Controller ]
     ▲                                                   │
     │ 6. Link to Outline                                │ 2. Start the job
     │                                                   ▼
     │                                      [ Jitsi Headless Recorder ]
     │                                      (Node.js + puppeteer-stream)
     │                                                   │
     │                                                   │ 3. Join Jitsi as a bot
     │                                                   ▼
     │                                          [ meet.jit.si / Jitsi ]
     │                                                   │
     │                                                   │ 4. Record audio (audio.wav)
     │                                                   ▼
     │                                           [ Transcriber ]
     │                                        (faster-whisper on CPU)
     │                                                   │
     │               5. Create the document              ▼
  [ Outline ] ◄────────────────────────────── [ Outline Publisher ]
```

---

## 🔬 Component research findings

### 1. Joining a Jitsi call and recording its audio
* **Why not Jitsi Jibri:** Jibri is hard-wired to the internal XMPP control protocol of its own Jitsi server. For joining the public `meet.jit.si` as a guest it is unusable and excessively heavy.
* **Chosen approach: headless Chromium + `puppeteer-stream` (Node.js)**
  * It lets us capture the page's incoming audio stream directly through the Chrome DevTools / Extension API, with no need to run heavy virtual displays (Xvfb) and virtual audio servers (PulseAudio).
  * **Automatic join without clicking through the UI:** Jitsi settings are passed straight in the URL hash:
    ```text
    https://meet.jit.si/<ROOM_ID>#config.prejoinConfig.enabled=false&config.startWithAudioMuted=true&config.startWithVideoMuted=true&userInfo.displayName="🎙️ Transcriber"
    ```
    This disables the pre-join screen (Lobby) and turns off the bot's microphone/camera, removing any dependency on changes to the Jitsi UI.
  * **Detecting the end of the meeting:** The script tracks the participant count via DOM selectors or the Jitsi API. If the bot is the only one left in the room (or on a silence timeout / a stop command), the recording is finalized.

### 2. Local transcription on CPU
* **Engine:** [`faster-whisper`](https://github.com/SYSTRAN/faster-whisper) (built on CTranslate2).
* **CPU configuration:**
  * `device="cpu"`
  * `compute_type="int8"` — reduces memory usage and speeds up inference on CPU by 2–4x with no loss of quality.
  * Model: `large-v3` for the best quality on Russian and English (or `medium` for faster runs).
* **Formatting:** The script splits the audio into segments with timecodes:
  ```markdown
  [00:05] Hi everyone, let's start with the architecture discussion...
  [00:18] On the second point, I propose the following solution...
  ```

### 3. Export to Outline
* Outline offers a simple REST API.
* Endpoint: `POST /api/documents.create`
* Authorization: `Authorization: Bearer <OUTLINE_API_KEY>`
* Payload:
  ```json
  {
    "collectionId": "<COLLECTION_ID>",
    "title": "Meeting: <Zulip topic name> (<Date>)",
    "text": "# Meeting transcript\n\n**Date:** 2026-09-13\n**Zulip topic:** ...\n\n---\n\n## Transcript\n...",
    "publish": true
  }
  ```
* The API response returns `data.url` (or the document path), which forms the link to send to Zulip.

### 4. Zulip integration and automatic bot joining

#### Activation scenario: an unobtrusive semi-automatic flow via emoji reactions
So as not to clutter chats with extra service messages and not to record accidental short calls, the following mechanics were adopted:

1. **Call detection:**
   * The Zulip bot is subscribed to the event stream (Events API).
   * When someone starts a video call, Zulip posts a system message with a link to `meet.jit.si/...`.
   * The bot intercepts that message and extracts the room URL and the message ID.
2. **A quiet offer to record (without spamming the chat):**
   * The bot **does not post any text messages into the topic**.
   * Instead, the bot immediately **places a 🎙️ (or 🔴) emoji reaction** on the call-start message itself via the Zulip API (`POST /messages/{message_id}/reactions`).
3. **Joining on a click:**
   * If any participant wants a transcript of the meeting, they simply click the already-placed 🎙️ reaction (or add it).
   * The bot watches for the `reaction: add` event on that message:
     * It starts the background Puppeteer recorder container/process with the extracted URL.
     * To confirm that recording has started, the bot can add a 🔴 reaction to the message (a visual recording indicator, no text).
4. **If nobody clicks the reaction:**
   * The bot does nothing, the call is not recorded, and no server resources are spent.
5. **Completion and publication:**
   * When all participants leave Jitsi, the recorder finishes its work.
   * `faster-whisper` transcribes the recording on CPU.
   * The document is created in Outline.
   * Only then does the bot post the final message with the link to the transcript into the Zulip topic:
     ```markdown
     🎙️ **The meeting transcript is ready!**

     📄 Document in Outline: [Open the transcript](https://outline.your-domain.com/doc/...)

     <details>
     <summary>Short preview</summary>

     [00:00] ...
     </details>
     ```

---

## 📁 Project structure (recommended template)

```text
jitsi2outline/
├── README.md                 # Documentation and specification (this file)
├── docker-compose.yml        # Service orchestration
├── .env.example              # Example environment variables
├── recorder/                 # Call recording module (Node.js + Puppeteer)
│   ├── Dockerfile
│   ├── package.json
│   └── record.js             # Joins Jitsi and records audio.wav
├── transcriber/              # Transcription and integrations module (Python)
│   ├── Dockerfile
│   ├── requirements.txt
│   ├── transcribe.py         # faster-whisper processing
│   ├── outline_client.py     # Client for the Outline API
│   ├── zulip_bot.py          # Zulip bot for receiving commands and sending results
│   └── pipeline.py           # Orchestrator: recording -> transcript -> outline -> zulip
└── data/                     # Temporary storage for audio files (volume)
```

---

## 📋 Tasks for the developer agent

1. **Stage 1: Recorder (`recorder/record.js`)**
   - Implement the Puppeteer script with `puppeteer-stream`.
   - Verify joining via a test Jitsi link with the URL flags (prejoin disabled, audio/video muted).
   - Record the audio stream into an `audio.wav` file.
   - Finish the recording when participants leave or on an external signal.

2. **Stage 2: Transcriber (`transcriber/transcribe.py`)**
   - Configure `faster-whisper` on CPU with `int8`.
   - A function that builds the Markdown text with timecodes.

3. **Stage 3: Outline Client (`transcriber/outline_client.py`)**
   - Implement the method that creates a document in Outline via the REST API.
   - Error handling and retrieval of the public/internal link.

4. **Stage 4: Zulip Bot & Pipeline (`transcriber/zulip_bot.py`)**
   - Implement handling of incoming messages: parsing the Jitsi call URL.
   - Starting the Puppeteer container/process.
   - Sending the final message with the result into the thread.

5. **Stage 5: Docker Compose**
   - Package everything into a single `docker-compose.yml` for deployment on a local server.
