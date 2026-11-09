// Playground: sends the text to POST /v1/filter as the user types and shows
// the censored result, the detected words and where the processing time
// lands on a latency scale. No framework, no build step.
"use strict";

const $ = (sel) => document.querySelector(sel);

const els = {
  input: $("#input"),
  languages: $("#languages"),
  maskChar: $("#mask-char"),
  status: $("#status"),
  output: $("#output"),
  error: $("#error"),
  matches: $("#matches"),
  matchesBody: $("#matches tbody"),
  tAlgo: $("#t-algo"),
  tRtt: $("#t-rtt"),
  mAlgo: $("#m-algo"),
  mRtt: $("#m-rtt"),
  raw: $("#raw"),
};

const DEBOUNCE_MS = 60;
let seq = 0;       // id of the latest request; older answers are ignored
let timer = null;

// ------------------------------------------------------------- formatting

const number = new Intl.NumberFormat("pt-BR", { maximumFractionDigits: 1 });

function formatNs(ns) {
  if (ns < 1e3) return `${number.format(ns)} ns`;
  if (ns < 1e6) return `${number.format(ns / 1e3)} µs`;
  if (ns < 1e9) return `${number.format(ns / 1e6)} ms`;
  return `${number.format(ns / 1e9)} s`;
}

// Position on the ruler: log10 from 1 ns (0) to 1 s (1).
function rulerX(ns) {
  const x = Math.log10(Math.max(ns, 1)) / 9;
  return Math.min(Math.max(x, 0), 1);
}

function placeMarker(marker, ns) {
  marker.style.setProperty("--x", rulerX(ns).toFixed(4));
  marker.hidden = false;
}

// ------------------------------------------------------------- options

function selectedLanguages() {
  return [...els.languages.querySelectorAll("input:checked")].map((i) => i.value);
}

function maskOptions() {
  const mode = document.querySelector('input[name="mask"]:checked').value;
  const char = Array.from(els.maskChar.value.trim())[0] || "*";
  return { mask: mode, mask_char: char };
}

async function loadLanguages() {
  try {
    const res = await fetch("/v1/languages");
    const data = await res.json();
    const defaults = new Set(data.default);
    els.languages.replaceChildren(
      ...data.languages.map((lang) => {
        const label = document.createElement("label");
        label.className = "chip";
        const input = document.createElement("input");
        input.type = "checkbox";
        input.value = lang.code;
        input.checked = defaults.has(lang.code);
        const span = document.createElement("span");
        span.textContent = lang.code;
        if (lang.opt_in) {
          const small = document.createElement("small");
          small.textContent = " opcional";
          span.append(small);
        }
        label.title = `${lang.words} palavras`;
        label.append(input, span);
        return label;
      }),
    );
  } catch {
    els.languages.textContent = "Não foi possível carregar os idiomas.";
  }
}

// ------------------------------------------------------------- request

function schedule() {
  clearTimeout(timer);
  timer = setTimeout(run, DEBOUNCE_MS);
}

async function run() {
  const id = ++seq;
  const languages = selectedLanguages();
  if (languages.length === 0) {
    showError("Escolha ao menos um idioma.");
    return;
  }

  const body = { text: els.input.value, languages, ...maskOptions() };
  const started = performance.now();
  let res, data;
  try {
    res = await fetch("/v1/filter", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    data = await res.json();
  } catch {
    if (id === seq) showError("O servidor não respondeu. Confira se ele está rodando.");
    return;
  }
  const rttNs = (performance.now() - started) * 1e6;
  if (id !== seq) return; // a newer request is already on its way

  els.raw.textContent = JSON.stringify(data, null, 2);
  if (!res.ok) {
    showError(data.error || `Erro ${res.status}`);
    return;
  }
  render(data, rttNs);
}

// ------------------------------------------------------------- rendering

function showError(message) {
  els.error.textContent = message;
  els.error.hidden = false;
}

function render(data, rttNs) {
  els.error.hidden = true;

  const n = data.matches.length;
  els.status.dataset.state = n ? "profane" : "clean";
  els.status.textContent = n === 0
    ? "Nenhum palavrão"
    : n === 1 ? "1 palavrão encontrado" : `${n} palavrões encontrados`;

  // Offsets are in characters (code points), and masking keeps the length,
  // so they index censored_text directly.
  const chars = Array.from(data.censored_text);
  const parts = [];
  let pos = 0;
  for (const m of data.matches) {
    if (m.start > pos) parts.push(document.createTextNode(chars.slice(pos, m.start).join("")));
    const mark = document.createElement("mark");
    mark.textContent = chars.slice(m.start, m.end).join("");
    mark.title = `${m.text} → ${m.word}`;
    parts.push(mark);
    pos = m.end;
  }
  if (pos < chars.length) parts.push(document.createTextNode(chars.slice(pos).join("")));
  els.output.replaceChildren(...parts);

  els.matches.hidden = n === 0;
  els.matchesBody.replaceChildren(
    ...data.matches.map((m) => {
      const tr = document.createElement("tr");
      for (const value of [m.text, m.word, m.languages.join(", "), `${m.start}–${m.end}`]) {
        const td = document.createElement("td");
        td.textContent = value;
        tr.append(td);
      }
      return tr;
    }),
  );

  els.tAlgo.textContent = formatNs(data.elapsed_time_ns);
  els.tRtt.textContent = formatNs(rttNs);
  placeMarker(els.mAlgo, data.elapsed_time_ns);
  placeMarker(els.mRtt, rttNs);
}

// ------------------------------------------------------------- wiring

els.input.addEventListener("input", schedule);
els.maskChar.addEventListener("input", schedule);
document.addEventListener("change", (e) => {
  if (e.target.matches('input[name="mask"], #languages input')) run();
});

for (const button of document.querySelectorAll("[data-example]")) {
  button.addEventListener("click", () => {
    els.input.value = button.dataset.example;
    if (button.dataset.languages) {
      const wanted = new Set(button.dataset.languages.split(","));
      for (const input of els.languages.querySelectorAll("input")) {
        input.checked = wanted.has(input.value);
      }
    }
    els.input.focus();
    run();
  });
}

loadLanguages().then(run);
