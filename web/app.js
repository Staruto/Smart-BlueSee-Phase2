const startBtn = document.getElementById('startBtn');
const stopBtn = document.getElementById('stopBtn');
const statusDiv = document.getElementById('status');
const remoteAudio = document.getElementById('remoteAudio');

let pc = null;
let ws = null;
let localStream = null;

startBtn.onclick = async () => {
    startBtn.disabled = true;
    updateStatus('Requesting microphone permissions...');
    
    try {
        localStream = await navigator.mediaDevices.getUserMedia({ audio: true, video: false });
    } catch (e) {
        alert('Failed to access microphone: ' + e);
        startBtn.disabled = false;
        return;
    }

    updateStatus('Connecting to signaling server...');
    
    // Connect to the Go WebSocket signaling server
    ws = new WebSocket(getWebSocketUrl());
    
    ws.onopen = () => {
        updateStatus('Signaling server connected, negotiating WebRTC...');
        startWebRTC();
    };

    ws.onmessage = async (event) => {
        const msg = JSON.parse(event.data);
        if (msg.type === 'answer') {
            await pc.setRemoteDescription(new RTCSessionDescription(msg));
            updateStatus('WebRTC connection established!');
            stopBtn.disabled = false;
        } else if (msg.type === 'candidate') {
            await pc.addIceCandidate(new RTCIceCandidate(msg.candidate));
        }
    };

    ws.onerror = () => {
        updateStatus('Error connecting to signaling server!');
    };

    ws.onclose = () => {
        if (stopBtn.disabled) {
            updateStatus('Signaling server connection closed');
        }
    };
};

stopBtn.onclick = () => {
    if (pc) pc.close();
    if (ws) ws.close();
    if (localStream) localStream.getTracks().forEach(track => track.stop());
    
    startBtn.disabled = false;
    stopBtn.disabled = true;
    updateStatus('Connection disconnected');
};

async function startWebRTC() {
    // Create the basic RTCPeerConnection
    pc = new RTCPeerConnection({
        iceServers: [{ urls: 'stun:stun.l.google.com:19302' }]
    });

    // Handling Received Remote Audio Tracks (ESP32 Audio)
    pc.ontrack = (event) => {
        if (remoteAudio.srcObject !== event.streams[0]) {
            remoteAudio.srcObject = event.streams[0];
            remoteAudio.play().catch(e => console.error('Audio play error:', e));
        }
    };

    // When there is a local ICE candidate, send it to the server
    pc.onicecandidate = (event) => {
        if (event.candidate) {
            ws.send(JSON.stringify({
                type: 'candidate',
                candidate: event.candidate
            }));
        }
    };

    // Add a local microphone track to PeerConnection
    localStream.getTracks().forEach(track => pc.addTrack(track, localStream));

    // Create Offer and send to signaling server by WebSocket
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    
    ws.send(JSON.stringify(pc.localDescription));
}

function updateStatus(text) {
    statusDiv.textContent = 'status: ' + text;
    console.log(text);
}

function getWebSocketUrl() {
    const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
    return scheme + '://' + window.location.host + '/ws';
}
