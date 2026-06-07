

## Service related commands

### Start / Restart service
```bash
sudo systemctl status webrtc-client
sudo systemctl start webrtc-client

sudo systemctl status nginx
sudo systemctl start nginx

screen -ls # check old ngrok
screen -S ngrok # create new
ngrok http http://127.0.0.1:80 # ctrl + a, ctrl + d to detach
```

### Log
```bash
sudo journalctl -u webrtc-client -n 20 --no-paper
```