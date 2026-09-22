document.getElementById('title').innerText = "Hello from Embed FS!";
console.log("JS Loaded successfully");

setInterval(() => {
    window.chrome.webview.postMessage('www.shuyuz.com')
}, 3000)