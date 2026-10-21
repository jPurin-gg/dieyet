// Live barcode scanning with the camera and zxing-wasm (vendored, pinned).
// Only retail formats are decoded, so a QR code printed on the package is
// never mistaken for the JAN. A code must be read twice in a row to count.

import { prepareZXingModule, readBarcodes } from '/vendor/zxing-wasm/es/reader/index.js';

const FORMATS = ['EAN-13', 'EAN-8', 'UPC-A', 'UPC-E'];
const INTERVAL_MS = 120;

let modulePrepared = false;

function prepare() {
  if (modulePrepared) return;
  modulePrepared = true;
  prepareZXingModule({
    overrides: {
      locateFile: (path, prefix) => (path.endsWith('.wasm') ? '/vendor/zxing-wasm/zxing_reader.wasm' : prefix + path),
    },
    fireImmediately: true,
  });
}

/**
 * Starts the camera on `video` and calls `onCode(text)` once a barcode has been
 * confirmed. Returns a stop() function.
 */
export function startScanner(video, { onCode, onStatus }) {
  prepare();
  let stream = null;
  let stopped = false;
  let timer = null;
  let lastText = '';
  let streak = 0;
  const canvas = document.createElement('canvas');
  const ctx = canvas.getContext('2d', { willReadFrequently: true });

  const status = (msg) => onStatus && onStatus(msg);

  async function tick() {
    if (stopped) return;
    if (video.readyState >= 2 && video.videoWidth > 0) {
      // Decode only the middle band of the frame (where the guide box is),
      // downscaled so each pass stays cheap on a phone.
      const vw = video.videoWidth, vh = video.videoHeight;
      const sx = Math.floor(vw * 0.1), sw = Math.floor(vw * 0.8);
      const sh = Math.floor(vh * 0.4), sy = Math.floor((vh - sh) / 2);
      const scale = Math.min(1, 800 / sw);
      canvas.width = Math.floor(sw * scale);
      canvas.height = Math.floor(sh * scale);
      ctx.drawImage(video, sx, sy, sw, sh, 0, 0, canvas.width, canvas.height);
      try {
        const results = await readBarcodes(ctx.getImageData(0, 0, canvas.width, canvas.height), {
          formats: FORMATS,
          tryHarder: true,
          tryRotate: true,
          tryInvert: false,
          maxNumberOfSymbols: 1,
        });
        const hit = results.find((r) => r.isValid && r.text);
        if (hit) {
          streak = hit.text === lastText ? streak + 1 : 1;
          lastText = hit.text;
          status(`読み取り中… ${hit.text}`);
          if (streak >= 2) {
            stop();
            onCode(hit.text);
            return;
          }
        }
      } catch (e) {
        status('読み取りでエラーが起きました: ' + (e && e.message ? e.message : e));
      }
    }
    timer = setTimeout(tick, INTERVAL_MS);
  }

  function stop() {
    stopped = true;
    if (timer) clearTimeout(timer);
    if (stream) stream.getTracks().forEach((t) => t.stop());
    stream = null;
    video.srcObject = null;
  }

  (async () => {
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
      status('このブラウザではカメラを使えません（HTTPS が必要です）');
      return;
    }
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        audio: false,
        video: { facingMode: { ideal: 'environment' }, width: { ideal: 1280 }, height: { ideal: 720 } },
      });
    } catch (e) {
      status('カメラを使えませんでした。設定でカメラの許可を確認してください');
      return;
    }
    if (stopped) { stream.getTracks().forEach((t) => t.stop()); return; }
    video.srcObject = stream;
    try { await video.play(); } catch (e) { /* autoplay policies; the muted+playsinline video usually plays */ }
    status('バーコードを枠に入れてください');
    tick();
  })();

  return stop;
}
