// Multi-file uploader: validates size client-side, makes a small JPEG
// thumbnail in the browser (keeps CPU off the server), and uploads a few
// files at a time with per-file progress.
(() => {
  const root = document.getElementById("uploader");
  const input = document.getElementById("file-input");
  const list = document.getElementById("upload-list");
  const maxBytes = Number(root.dataset.maxBytes);
  const THUMB_PX = 480;
  const CONCURRENCY = 3;

  const queue = [];
  let active = 0;
  let succeeded = 0;

  input.addEventListener("change", () => {
    enqueue(input.files);
    input.value = "";
  });

  document.addEventListener("dragover", (e) => {
    e.preventDefault();
    document.body.classList.add("dragging");
  });
  document.addEventListener("dragleave", (e) => {
    if (e.relatedTarget === null) document.body.classList.remove("dragging");
  });
  document.addEventListener("drop", (e) => {
    e.preventDefault();
    document.body.classList.remove("dragging");
    enqueue(e.dataTransfer.files);
  });

  function enqueue(files) {
    for (const file of files) {
      const row = document.createElement("li");
      row.innerHTML = '<span class="name"></span><progress max="1" value="0"></progress><span class="status"></span>';
      row.querySelector(".name").textContent = file.name;
      list.appendChild(row);
      if (file.size > maxBytes) {
        fail(row, "over 10 MB");
        continue;
      }
      if (file.type && !file.type.startsWith("image/")) {
        fail(row, "not an image");
        continue;
      }
      queue.push({ file, row });
    }
    pump();
  }

  function pump() {
    while (active < CONCURRENCY && queue.length) {
      const job = queue.shift();
      active++;
      upload(job).finally(() => {
        active--;
        if (active === 0 && queue.length === 0) finished();
        else pump();
      });
    }
  }

  function finished() {
    if (succeeded > 0 && !list.querySelector("li.error")) {
      location.href = "/";
    } else if (succeeded > 0) {
      const a = document.createElement("a");
      a.href = "/";
      a.className = "button secondary";
      a.textContent = "Refresh gallery";
      list.appendChild(a);
    }
  }

  function fail(row, msg) {
    row.classList.add("error");
    row.querySelector(".status").textContent = msg;
    row.querySelector("progress").remove();
  }

  async function makeThumb(file) {
    try {
      const bmp = await createImageBitmap(file, { imageOrientation: "from-image" });
      const scale = Math.min(1, THUMB_PX / Math.max(bmp.width, bmp.height));
      const canvas = document.createElement("canvas");
      canvas.width = Math.round(bmp.width * scale);
      canvas.height = Math.round(bmp.height * scale);
      canvas.getContext("2d").drawImage(bmp, 0, 0, canvas.width, canvas.height);
      bmp.close();
      return await new Promise((res) => canvas.toBlob(res, "image/jpeg", 0.8));
    } catch {
      return null; // e.g. HEIC on browsers that can't decode it; server falls back to the original
    }
  }

  async function upload({ file, row }) {
    const status = row.querySelector(".status");
    const bar = row.querySelector("progress");
    status.textContent = "preparing…";
    const form = new FormData();
    const thumb = await makeThumb(file);
    if (thumb) form.append("thumb", thumb, "thumb.jpg");
    form.append("file", file, file.name);

    return new Promise((resolve) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", "/api/photos");
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) bar.value = e.loaded / e.total;
        status.textContent = "uploading…";
      };
      xhr.onload = () => {
        if (xhr.status === 201) {
          succeeded++;
          row.classList.add("done");
          bar.value = 1;
          status.textContent = "done";
        } else {
          let msg = "upload failed (" + xhr.status + ")";
          try { msg = JSON.parse(xhr.responseText).error || msg; } catch {}
          if (xhr.status === 401) msg = "signed out, reload the page";
          fail(row, msg);
        }
        resolve();
      };
      xhr.onerror = () => { fail(row, "network error"); resolve(); };
      xhr.send(form);
    });
  }
})();
