const startBtn = document.getElementById('startBtn');
const stopBtn = document.getElementById('stopBtn');
const statusDiv = document.getElementById('status');
const remoteAudio = document.getElementById('remoteAudio');

let pc = null;
let ws = null;
let localStream = null;

startBtn.onclick = async () => {
    startBtn.disabled = true;
    updateStatus('正在请求麦克风权限...');
    
    try {
        localStream = await navigator.mediaDevices.getUserMedia({ audio: true, video: false });
    } catch (e) {
        alert('获取麦克风失败: ' + e);
        startBtn.disabled = false;
        return;
    }

    updateStatus('正在连接信令服务器...');
    
    // 连接到 Go WebSocket 信令服务器
    ws = new WebSocket('ws://' + window.location.host + '/ws');
    
    ws.onopen = () => {
        updateStatus('信令服务器已连接，正在协商 WebRTC...');
        startWebRTC();
    };

    ws.onmessage = async (event) => {
        const msg = JSON.parse(event.data);
        if (msg.type === 'answer') {
            await pc.setRemoteDescription(new RTCSessionDescription(msg));
            updateStatus('WebRTC 连接已建立！');
            stopBtn.disabled = false;
        } else if (msg.type === 'candidate') {
            await pc.addIceCandidate(new RTCIceCandidate(msg.candidate));
        }
    };

    ws.onerror = (e) => {
        updateStatus('信令服务连接错误！');
    };
};

stopBtn.onclick = () => {
    if (pc) pc.close();
    if (ws) ws.close();
    if (localStream) localStream.getTracks().forEach(track => track.stop());
    
    startBtn.disabled = false;
    stopBtn.disabled = true;
    updateStatus('已断开连接');
};

async function startWebRTC() {
    // 创建基础的 RTCPeerConnection
    pc = new RTCPeerConnection({
        iceServers: [{ urls: 'stun:stun.l.google.com:19302' }]
    });

    // 接收到远端音轨时的处理（ESP32 的声音）
    pc.ontrack = (event) => {
        if (remoteAudio.srcObject !== event.streams[0]) {
            remoteAudio.srcObject = event.streams[0];
            remoteAudio.play().catch(e => console.error('Audio play error:', e));
        }
    };

    // 当有本地的 ICE 候选者时，发送给服务器
    pc.onicecandidate = (event) => {
        if (event.candidate) {
            ws.send(JSON.stringify({
                type: 'candidate',
                candidate: event.candidate
            }));
        }
    };

    // 将本地麦克风音轨添加到 PeerConnection
    localStream.getTracks().forEach(track => pc.addTrack(track, localStream));

    // 创建 Offer 并通过 WS 发送给服务器
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    
    ws.send(JSON.stringify(pc.localDescription));
}

function updateStatus(text) {
    statusDiv.textContent = '状态：' + text;
    console.log(text);
}
