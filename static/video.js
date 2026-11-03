// @license magnet:?xt=urn:btih:8e4f440f4c65981c5bf93c76d35135ba5064d8b7&dn=apache-2.0.txt Apache-2.0
// Plays idea pin videos (HLS with a separate audio rendition) with hls.js.
// Pages work without it: the video keeps its poster and a note says why it
// doesn't play. Only the small playlist loads before play is pressed, and
// nothing plays by itself.
(function () {
  "use strict";
  var videos = document.querySelectorAll("video[data-hls]");
  for (var i = 0; i < videos.length; i++) {
    setUp(videos[i]);
  }

  function setUp(video) {
    var src = video.getAttribute("data-hls");
    var note = document.getElementById(video.getAttribute("aria-describedby"));
    if (window.Hls && window.Hls.isSupported()) {
      // No worker: it would need a blob: script, which the page's policy
      // doesn't allow.
      var hls = new window.Hls({ enableWorker: false, autoStartLoad: false });
      hls.loadSource(src);
      hls.attachMedia(video);
      video.addEventListener("play", function () { hls.startLoad(); }, { once: true });
    } else if (video.canPlayType("application/vnd.apple.mpegurl")) {
      video.src = src; // Safari plays HLS natively
    } else {
      return; // keep the note
    }
    if (note) {
      note.hidden = true;
    }
  }
})();
// @license-end
