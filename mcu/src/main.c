#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "freertos/event_groups.h"
#include "esp_system.h"
#include "esp_log.h"
#include "nvs_flash.h"
#include "esp_event.h"
#include "esp_netif.h"
#include "esp_wifi.h"
#include "driver/i2s.h"
#include "driver/gpio.h"
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <unistd.h>
#include "esp_afe_sr_iface.h"
#include "esp_afe_sr_models.h"
#include "esp_afe_config.h"
#include <string.h>
#include <stdlib.h>
#include <errno.h>

static const char *TAG = "udp_pcmu_espidf";
static esp_afe_sr_data_t *afe_data = NULL;
static const esp_afe_sr_iface_t *afe_handle = NULL;
static int afe_chunksize = 0;
static int afe_channels = 0;
static int16_t *aec_input = NULL;
static int16_t *speaker_ref = NULL;
// --- Hardware pins ---
#define I2S_PORT      I2S_NUM_0
#define I2S_BCLK      7
#define I2S_WS        2
#define I2S_DATA_IN   3
#define I2S_DATA_OUT  1
#define SWITCH_GPIO   4

// --- Network / UDP ---
#define WIFI_SSID      "yuhao"
#define WIFI_PASS      "12345678"
#define UDP_SERVER_IP  "117.72.24.77"
#define UDP_SERVER_PORT 5000
#define UDP_LOCAL_PORT  5000
#define UDP_HEARTBEAT_MS 2000
#define UDP_STATUS_LOG_MS 5000
#define UDP_HEARTBEAT_BYTES 160
#define UDP_HEARTBEAT_ACK_0 'H'
#define UDP_HEARTBEAT_ACK_1 'B'

// --- Audio ---
#define SAMPLE_RATE    16000
#define UDP_AUDIO_RATE 8000
#define I2S_DMA_BUF_COUNT 8
#define I2S_DMA_BUF_LEN   256
#define FRAME_MS          20
#define MIC_GAIN          6
#define VAD_SILENCE_HOLD_MS 1200
#define VAD_START_CONSECUTIVE_FRAMES 5
#define VAD_VOLUME_THRESHOLD_DB -32.0f
#define VAD_DEBUG_LOG_MS 2000
#define MAX_ULAW_PACKET_BYTES 1024


static EventGroupHandle_t s_wifi_event_group;
const int WIFI_CONNECTED_BIT = BIT0;

static void wifi_event_handler(void* arg, esp_event_base_t event_base,
                               int32_t event_id, void* event_data) {
    if (event_base == WIFI_EVENT && event_id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (event_base == IP_EVENT && event_id == IP_EVENT_STA_GOT_IP) {
        ip_event_got_ip_t *event = (ip_event_got_ip_t *)event_data;
        ESP_LOGI(TAG, "WiFi got IP: " IPSTR, IP2STR(&event->ip_info.ip));
        xEventGroupSetBits(s_wifi_event_group, WIFI_CONNECTED_BIT);
    } else if (event_base == WIFI_EVENT && event_id == WIFI_EVENT_STA_DISCONNECTED) {
        ESP_LOGW(TAG, "WiFi disconnected, reconnecting");
        esp_wifi_connect();
        xEventGroupClearBits(s_wifi_event_group, WIFI_CONNECTED_BIT);
    }
}

static void wifi_init_sta(void) {
    s_wifi_event_group = xEventGroupCreate();
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    esp_netif_create_default_wifi_sta();

    wifi_init_config_t cfg = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&cfg));

    ESP_ERROR_CHECK(esp_event_handler_instance_register(WIFI_EVENT,
                                                        ESP_EVENT_ANY_ID,
                                                        &wifi_event_handler,
                                                        NULL,
                                                        NULL));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(IP_EVENT,
                                                        IP_EVENT_STA_GOT_IP,
                                                        &wifi_event_handler,
                                                        NULL,
                                                        NULL));

    wifi_config_t wifi_config = {0};
    strncpy((char*)wifi_config.sta.ssid, WIFI_SSID, sizeof(wifi_config.sta.ssid)-1);
    strncpy((char*)wifi_config.sta.password, WIFI_PASS, sizeof(wifi_config.sta.password)-1);

    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &wifi_config));
    ESP_ERROR_CHECK(esp_wifi_start());
    ESP_ERROR_CHECK(esp_wifi_set_ps(WIFI_PS_NONE));

    ESP_LOGI(TAG, "wifi_init_sta finished. Connecting to SSID:%s", WIFI_SSID);
}

static void i2s_init(void) {
    i2s_config_t i2s_config = {
        .mode = (i2s_mode_t)(I2S_MODE_MASTER | I2S_MODE_TX | I2S_MODE_RX),
        .sample_rate = SAMPLE_RATE,
        .bits_per_sample = I2S_BITS_PER_SAMPLE_32BIT,
        .channel_format = I2S_CHANNEL_FMT_ONLY_LEFT,
        .communication_format = I2S_COMM_FORMAT_STAND_I2S, 
        .intr_alloc_flags = ESP_INTR_FLAG_LEVEL1,
        .dma_buf_count = I2S_DMA_BUF_COUNT,
        .dma_buf_len = I2S_DMA_BUF_LEN,
        .use_apll = false,
        .tx_desc_auto_clear = true,
    };

    i2s_pin_config_t pin_config = {
        .mck_io_num = I2S_PIN_NO_CHANGE,
        .bck_io_num = I2S_BCLK,
        .ws_io_num = I2S_WS,
        .data_out_num = I2S_DATA_OUT,
        .data_in_num = I2S_DATA_IN,
    };

    ESP_ERROR_CHECK(i2s_driver_install(I2S_PORT, &i2s_config, 0, NULL));
    ESP_ERROR_CHECK(i2s_set_pin(I2S_PORT, &pin_config));
    ESP_ERROR_CHECK(i2s_zero_dma_buffer(I2S_PORT));
}

static void aec_init(void)
{
    srmodel_list_t *models = esp_srmodel_init("model");
    afe_config_t *cfg = afe_config_init("MR",models,AFE_TYPE_FD,AFE_MODE_HIGH_PERF);

    cfg->aec_init = true;
    cfg->vad_init = true;
    cfg->vad_mode = VAD_MODE_1;
    cfg->vad_min_speech_ms = 256;
    cfg->vad_min_noise_ms = VAD_SILENCE_HOLD_MS;
    cfg->vad_delay_ms = 128;
    cfg->vad_mute_playback = false;
    cfg->wakenet_init = false;
    cfg->agc_init = false;
    cfg->se_init = false;
    cfg = afe_config_check(cfg);
    afe_handle =
        esp_afe_handle_from_config(cfg);
    afe_data =
        afe_handle->create_from_config(cfg);
    if (!afe_data)
    {
        ESP_LOGE(TAG,
                 "AFE create failed");
        return;
    }

    afe_chunksize =
        afe_handle->get_feed_chunksize(
            afe_data
        );

    afe_channels =
        afe_handle->get_feed_channel_num(
            afe_data
        );


    ESP_LOGI(
        TAG,
        "AEC chunk=%d channels=%d",
        afe_chunksize,
        afe_channels
    );


    aec_input =
        malloc(
            sizeof(int16_t) *
            afe_chunksize *
            afe_channels
        );


    speaker_ref =
        calloc(
            afe_chunksize,
            sizeof(int16_t)
        );


    if (!aec_input || !speaker_ref)
    {
        ESP_LOGE(
            TAG,
            "AEC buffer malloc failed"
        );
    }
}

// G.711 u-law
#define ULAW_BIAS 0x84
#define ULAW_CLIP 32635

static uint8_t encode_ulaw(int16_t sample) {
    static const int exp_lut[256] = { /* omitted init for brevity, we'll reuse standard table */
        0,0,1,1,2,2,2,2,3,3,3,3,3,3,3,3,
        4,4,4,4,4,4,4,4,4,4,4,4,4,4,4,4,
        5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,
        5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,
        6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,
        6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,
        6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,
        6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7
    };
    int sign, exponent, mantissa;
    uint8_t ulawbyte;

    sign = (sample >> 8) & 0x80;
    if (sign != 0) sample = -sample;
    if (sample > ULAW_CLIP) sample = ULAW_CLIP;

    sample = sample + ULAW_BIAS;
    exponent = exp_lut[(sample >> 7) & 0xFF];
    mantissa = (sample >> (exponent + 3)) & 0x0F;
    ulawbyte = ~(sign | (exponent << 4) | mantissa);

    if (ulawbyte == 0) ulawbyte = 0x02;

    return ulawbyte;
}

static int16_t decode_ulaw(uint8_t ulawbyte) {
    static const int exp_lut[8] = {0,132,396,924,1980,4092,8316,16764};
    int sign, exponent, mantissa, sample;

    ulawbyte = ~ulawbyte;
    sign = (ulawbyte & 0x80);
    exponent = (ulawbyte >> 4) & 0x07;
    mantissa = ulawbyte & 0x0F;

    sample = exp_lut[exponent] + (mantissa << (exponent + 3));
    if (sign != 0) sample = -sample;
    return (int16_t)sample;
}

static int send_pcm_as_ulaw(int sock,
                            const struct sockaddr_in *server_addr,
                            const int16_t *pcm,
                            int samples,
                            uint8_t *ulaw_buf,
                            size_t ulaw_buf_size)
{
    if (!pcm || samples <= 0 || ulaw_buf_size == 0) {
        return 0;
    }

    const int step = SAMPLE_RATE / UDP_AUDIO_RATE;
    if (step <= 0 || (SAMPLE_RATE % UDP_AUDIO_RATE) != 0) {
        ESP_LOGE(TAG, "Unsupported SAMPLE_RATE=%d UDP_AUDIO_RATE=%d",
                 SAMPLE_RATE, UDP_AUDIO_RATE);
        return -1;
    }

    int offset = 0;
    int bytes_sent = 0;
    while (offset < samples) {
        int input_chunk = samples - offset;
        int max_input = (int)ulaw_buf_size * step;
        if (input_chunk > max_input) {
            input_chunk = max_input;
        }

        int output_count = 0;
        for (int i = 0; i + step - 1 < input_chunk; i += step) {
            int32_t mixed = 0;
            for (int j = 0; j < step; j++) {
                mixed += pcm[offset + i + j];
            }
            mixed /= step;
            ulaw_buf[output_count++] = encode_ulaw((int16_t)mixed);
        }

        if (output_count == 0) {
            break;
        }

        ssize_t sent =
            sendto(sock,
                   ulaw_buf,
                   output_count,
                   0,
                   (const struct sockaddr *)server_addr,
                   sizeof(*server_addr));

        if (sent < 0) {
            ESP_LOGE(TAG, "UDP send audio failed, errno=%d", errno);
            return bytes_sent > 0 ? bytes_sent : -1;
        }

        bytes_sent += sent;
        offset += input_chunk;
    }

    return bytes_sent;
}

static int send_udp_heartbeat(int sock,
                              const struct sockaddr_in *server_addr,
                              uint8_t *ulaw_buf,
                              size_t ulaw_buf_size)
{
    int bytes = UDP_HEARTBEAT_BYTES;
    if (bytes > (int)ulaw_buf_size) {
        bytes = (int)ulaw_buf_size;
    }

    memset(ulaw_buf, 0xFF, bytes);

    ssize_t sent =
        sendto(sock,
               ulaw_buf,
               bytes,
               0,
               (const struct sockaddr *)server_addr,
               sizeof(*server_addr));

    if (sent < 0) {
        ESP_LOGE(TAG, "UDP heartbeat send failed, errno=%d", errno);
        return -1;
    }

    return (int)sent;
}

// Always enable recording (no GPIO switch in this build)


static void udp_task(void *arg) {
    (void)arg;
    int sock = -1;
    struct sockaddr_in server_addr = {0};
    struct sockaddr_in local_addr = {0};
    sock = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (sock < 0) {
        ESP_LOGE(TAG, "UDP socket failed, errno=%d", errno);
        vTaskDelete(NULL);
        return;
    }

    local_addr.sin_family = AF_INET;
    local_addr.sin_addr.s_addr = htonl(INADDR_ANY);
    local_addr.sin_port = htons(UDP_LOCAL_PORT);
    if (bind(sock, (struct sockaddr *)&local_addr, sizeof(local_addr)) < 0) {
        ESP_LOGE(TAG, "UDP bind local port %d failed, errno=%d",
                 UDP_LOCAL_PORT, errno);
        close(sock);
        vTaskDelete(NULL);
        return;
    }

    server_addr.sin_family = AF_INET;
    server_addr.sin_port = htons(UDP_SERVER_PORT);
    if (inet_aton(UDP_SERVER_IP, &server_addr.sin_addr) == 0) {
        ESP_LOGE(TAG, "Invalid UDP server IP: %s", UDP_SERVER_IP);
        close(sock);
        vTaskDelete(NULL);
        return;
    }

    ESP_LOGI(TAG,
             "UDP ready: local port=%d, remote=%s:%d",
             UDP_LOCAL_PORT,
             UDP_SERVER_IP,
             UDP_SERVER_PORT);
    ESP_LOGI(TAG,
             "UDP is connectionless; server is confirmed only after RX packet");

    int32_t *i2s_read_buf =
        malloc(sizeof(int32_t) * afe_chunksize);
    uint8_t rx_buf[MAX_ULAW_PACKET_BYTES];
    uint8_t ulaw_buf[MAX_ULAW_PACKET_BYTES];
    bool user_speaking = false;
    bool server_rx_seen = false;
    TickType_t last_speech_tick = 0;
    TickType_t last_heartbeat_tick = 0;
    TickType_t last_status_tick = 0;
    TickType_t last_vad_debug_tick = 0;
    int consecutive_speech_frames = 0;
    uint32_t tx_audio_packets = 0;
    uint32_t tx_heartbeat_packets = 0;
    uint32_t rx_packets = 0;

    if (!i2s_read_buf || !aec_input || !speaker_ref) {
        ESP_LOGE(TAG, "Audio buffers are not ready");
        close(sock);
        free(i2s_read_buf);
        vTaskDelete(NULL);
        return;
    }

    while (1) {
    TickType_t loop_now = xTaskGetTickCount();
    if ((loop_now - last_heartbeat_tick) >= pdMS_TO_TICKS(UDP_HEARTBEAT_MS)) {
        int sent =
            send_udp_heartbeat(sock,
                               &server_addr,
                               ulaw_buf,
                               sizeof(ulaw_buf));
        if (sent > 0) {
            tx_heartbeat_packets++;
            ESP_LOGI(TAG,
                     "UDP heartbeat sent: %d bytes to %s:%d",
                     sent,
                     UDP_SERVER_IP,
                     UDP_SERVER_PORT);
        }
        last_heartbeat_tick = loop_now;
    }

    if ((loop_now - last_status_tick) >= pdMS_TO_TICKS(UDP_STATUS_LOG_MS)) {
        ESP_LOGI(TAG,
                 "UDP status: target=%s:%d, server_rx=%s, audio_tx=%lu, heartbeat_tx=%lu, rx=%lu",
                 UDP_SERVER_IP,
                 UDP_SERVER_PORT,
                 server_rx_seen ? "yes" : "no",
                 (unsigned long)tx_audio_packets,
                 (unsigned long)tx_heartbeat_packets,
                 (unsigned long)rx_packets);
        last_status_tick = loop_now;
    }

    size_t bytes_read = 0;


    esp_err_t r =
        i2s_read(
            I2S_PORT,
            i2s_read_buf,
            afe_chunksize * sizeof(int32_t),
            &bytes_read,
            pdMS_TO_TICKS(100)
        );


    if (r == ESP_OK &&
        bytes_read ==
        afe_chunksize * sizeof(int32_t))
    {

        for(int i = 0;
            i < afe_chunksize;
            i++)
        {
            int32_t mic =
                i2s_read_buf[i] >> 16;
            mic *= MIC_GAIN;
            if(mic > 32767)
                mic = 32767;
            if(mic < -32768)
                mic = -32768;
            aec_input[2*i] =
                (int16_t)mic;
            aec_input[2*i + 1] =
                speaker_ref[i];
        }
        afe_handle->feed(
            afe_data,
            aec_input
        );
        afe_fetch_result_t *result =
            afe_handle->fetch(
                afe_data
            );


        if(result &&
        result->data &&
        result->data_size > 0)
        {
            TickType_t now = xTaskGetTickCount();
            bool vad_speech =
                (result->vad_state == VAD_SPEECH) &&
                (result->data_volume >= VAD_VOLUME_THRESHOLD_DB);

            int samples =
                result->data_size /
                sizeof(int16_t);

            if (vad_speech) {
                last_speech_tick = now;
                if (consecutive_speech_frames < VAD_START_CONSECUTIVE_FRAMES) {
                    consecutive_speech_frames++;
                }

                if (!user_speaking &&
                    consecutive_speech_frames >= VAD_START_CONSECUTIVE_FRAMES) {
                    user_speaking = true;
                    i2s_zero_dma_buffer(I2S_PORT);
                    memset(speaker_ref,
                           0,
                           sizeof(int16_t) * afe_chunksize);
                    ESP_LOGI(TAG,
                             "Voice detected: pause playback, volume=%.1f dB, frames=%d",
                             result->data_volume,
                             consecutive_speech_frames);

                    if (result->vad_cache &&
                        result->vad_cache_size > 0) {
                        int sent =
                            send_pcm_as_ulaw(
                                sock,
                                &server_addr,
                                result->vad_cache,
                                result->vad_cache_size / sizeof(int16_t),
                                ulaw_buf,
                                sizeof(ulaw_buf)
                            );
                        if (sent > 0) {
                            tx_audio_packets++;
                        }
                    }
                }
            } else if (user_speaking &&
                       (now - last_speech_tick) >
                           pdMS_TO_TICKS(VAD_SILENCE_HOLD_MS)) {
                user_speaking = false;
                consecutive_speech_frames = 0;
                ESP_LOGI(TAG, "Voice ended, resume playback");
            } else if (!user_speaking) {
                consecutive_speech_frames = 0;
            }

            if ((now - last_vad_debug_tick) >= pdMS_TO_TICKS(VAD_DEBUG_LOG_MS)) {
                ESP_LOGI(TAG,
                         "VAD debug: raw=%s, accepted=%s, volume=%.1f dB, frames=%d",
                         result->vad_state == VAD_SPEECH ? "speech" : "silence",
                         vad_speech ? "yes" : "no",
                         result->data_volume,
                         consecutive_speech_frames);
                last_vad_debug_tick = now;
            }

            if (user_speaking) {
                int sent =
                    send_pcm_as_ulaw(
                        sock,
                        &server_addr,
                        result->data,
                        samples,
                        ulaw_buf,
                        sizeof(ulaw_buf)
                    );
                if (sent > 0) {
                    tx_audio_packets++;
                }
            }
        }
    }

        // Use MSG_DONTWAIT to prevent hanging, and loop to drain buffer of all incoming WebRTC packets (which arrive every 20ms)
        while (1)
        {
            ssize_t len =
                recvfrom(
                    sock,
                    rx_buf,
                    sizeof(rx_buf),
                    MSG_DONTWAIT,
                    NULL,
                    NULL
                );

            if (len <= 0)
            {
                break;
            }

            rx_packets++;
            if (len == 2 &&
                rx_buf[0] == UDP_HEARTBEAT_ACK_0 &&
                rx_buf[1] == UDP_HEARTBEAT_ACK_1)
            {
                if (!server_rx_seen)
                {
                    server_rx_seen = true;
                    ESP_LOGI(TAG,
                             "UDP server heartbeat ACK received from %s:%d",
                             UDP_SERVER_IP,
                             UDP_SERVER_PORT);
                }
                continue;
            }

            if (!server_rx_seen)
            {
                server_rx_seen = true;
                ESP_LOGI(TAG,
                         "UDP server RX confirmed: first packet len=%d from %s:%d",
                         (int)len,
                         UDP_SERVER_IP,
                         UDP_SERVER_PORT);
            }

            if (user_speaking)
            {
                continue;
            }

            int32_t i2s_out[MAX_ULAW_PACKET_BYTES * (SAMPLE_RATE / UDP_AUDIO_RATE)];

            if (len > MAX_ULAW_PACKET_BYTES)
            {
                len = MAX_ULAW_PACKET_BYTES;
            }

            int out_count = 0;
            int upsample = SAMPLE_RATE / UDP_AUDIO_RATE;
            if (upsample <= 0) {
                upsample = 1;
            }

            for (int i = 0; i < len; i++)
            {
                int32_t sample =
                    decode_ulaw(rx_buf[i]);
                sample *= 12;
                if (sample > 32767)
                    sample = 32767;
                if (sample < -32768)
                    sample = -32768;
                if (i < afe_chunksize)
                {
                    speaker_ref[i] =
                        (int16_t)sample;
                }

                for (int j = 0;
                     j < upsample &&
                     out_count < (int)(sizeof(i2s_out) / sizeof(i2s_out[0]));
                     j++)
                {
                    i2s_out[out_count++] =
                        sample << 16;
                }
            }

            size_t bytes_written = 0;

            i2s_write(
                I2S_PORT,
                i2s_out,
                out_count * sizeof(int32_t),
                &bytes_written,
                portMAX_DELAY
            );

            static int dbg_rx_cnt = 0;

            if ((dbg_rx_cnt++ % 100) == 0)
            {
                ESP_LOGI(
                    TAG,
                    "Playing audio %d bytes",
                    (int)len
                );
            }
        }
    }
    close(sock);
    free(i2s_read_buf);
    vTaskDelete(NULL);
}

void app_main(void) {
    esp_err_t ret = nvs_flash_init();
    if (ret == ESP_ERR_NVS_NO_FREE_PAGES || ret == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        ret = nvs_flash_init();
    }
    ESP_ERROR_CHECK(ret);

    wifi_init_sta();
    i2s_init();
    aec_init();

    // wait for wifi
    xEventGroupWaitBits(s_wifi_event_group, WIFI_CONNECTED_BIT, false, true, portMAX_DELAY);
    ESP_LOGI(TAG, "connected to WiFi, app started");

    // Start UDP task only after WiFi is connected
    xTaskCreate(&udp_task, "udp_audio", 16384, NULL, 6, NULL);
}
