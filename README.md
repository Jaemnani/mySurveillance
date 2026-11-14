# mySurveillance

컴퓨터가 켜져 있는 동안 카메라 영상을 계속 저장하는 무료 CCTV 녹화 프로그램입니다.

- **카메라**: USB 카메라, IP 카메라(RTSP), 핸드폰 카메라 앱(RTSP 또는 HTTP/MJPEG 주소)
- **상시 녹화**: 영상 + 소리를 끊김 없이 저장하고, 카메라가 끊기거나 멈추면 자동으로 다시 연결
- **용량 관리**: 최대 저장 용량을 정하면, 가득 찼을 때 가장 오래된 녹화부터 자동 삭제
- **자동 시작**: 컴퓨터가 켜질 때 함께 실행
- **뷰어**: 웹 화면(폰 브라우저 포함)에서 실시간 보기, 지난 녹화 재생, 다운로드. 모바일 앱이 쓸 HTTP API 포함

지원 상태: **Windows 동작 확인**. macOS·Linux는 같은 코드로 빌드되지만 자동 시작 설치(launchd/systemd)는 아직 없고 실기 검증 전입니다.

## 사용법 (Windows)

1. `mysurveillance.exe` 를 실행합니다. 따로 설치할 것은 없습니다.
2. 브라우저에서 `http://localhost:8787` 을 엽니다.
3. 초기 비밀번호는 `C:\ProgramData\mySurveillance\initial-password.txt` 에 있습니다. 로그인 후 **설정**에서 바꾸세요.
4. **설정**에서 카메라를 추가하고 저장 폴더와 최대 용량을 정한 뒤 저장합니다.

### 컴퓨터가 켜질 때 자동으로 시작

| 명령 | 동작 |
| --- | --- |
| `mysurveillance.exe install` | Windows 서비스로 등록. 로그인하지 않아도 부팅 시 시작하고, 꺼지면 Windows가 다시 켭니다. 관리자 권한 터미널에서 실행. |
| `mysurveillance.exe install --user` | 현재 사용자가 로그인할 때 시작. 관리자 권한 불필요. |
| `mysurveillance.exe uninstall` | 자동 시작 해제 |

USB 카메라가 서비스 모드에서 잡히지 않으면(Windows 카메라 개인정보 설정의 영향) `install --user` 를 쓰세요.

### 카메라 종류

| 종류 | 입력 | 예 |
| --- | --- | --- |
| USB 카메라 | 목록에서 장치 선택 | 웹캠, 캡처 카드 |
| RTSP | `rtsp://아이디:비밀번호@주소:포트/경로` | 대부분의 IP 카메라, 폰의 RTSP 카메라 앱 |
| HTTP | `http://주소:포트/video` | 폰의 IP Webcam 앱(영상 `/video`, 소리 `/audio.wav`) |

RTSP 카메라는 원본 그대로 저장합니다(CPU 사용 거의 없음). 카메라가 H.265라서 폰에서 재생되지 않으면 해당 카메라의 "H.264로 변환"을 켜세요.

### 카메라 자동 검색

설정 화면의 **카메라 자동 검색**을 누르면 약 5초 동안 아래 방법으로 카메라를 찾아 목록으로 보여줍니다. **등록**을 누르면 초안이 만들어지고, 확인 후 **설정 저장**을 누르면 녹화가 시작됩니다.

| 방법 | 찾는 대상 | 다른 IP 대역 |
| --- | --- | --- |
| USB | 이 컴퓨터에 연결된 카메라 | - |
| ONVIF (WS-Discovery) | 대부분의 IP 카메라 | 같은 스위치/공유기 안이면 일부 검색 |
| Hikvision SADP | Hikvision 계열 | 검색됨 |
| Dahua DHDiscover | Dahua 계열 (Imou/Lechange 포함) | 검색됨 |
| 네트워크 스캔 | RTSP 서버, 폰 앱 IP Webcam(8080)·DroidCam(4747) | 같은 대역만 |

- 다른 대역 검색은 카메라가 같은 스위치/공유기(같은 랜)에 물려 있을 때만 가능합니다.
- 목록에는 이 컴퓨터의 네트워크 대역과, 카메라별 연결 가능 여부가 함께 표시됩니다. "연결 불가"는 카메라는 보이지만 대역이 달라 영상을 받을 수 없다는 뜻이며, 카메라 IP를 이 컴퓨터 대역으로 바꿔야 합니다.
- ONVIF 카메라는 아이디·비밀번호를 넣으면 영상 주소를 카메라에서 직접 받아옵니다. 그 외에는 제조사 기본 주소로 초안을 만듭니다.
- 폰 앱은 앱에서 서버를 시작한 상태이고 포트를 기본값으로 둔 경우에 검색됩니다.

### 집 밖에서 접속

비밀번호 로그인은 기본이지만, 인터넷에 직접 노출할 때는 반드시 암호화(HTTPS)된 경로를 쓰세요.

- **권장**: Tailscale 같은 사설망이나 Cloudflare Tunnel로 `http://localhost:8787` 을 연결 (공유기 포트 개방 불필요)
- 직접 HTTPS: `config.json` 의 `tlsCert`, `tlsKey` 에 인증서 파일 경로 지정
- 같은 와이파이의 다른 기기에서 접속하려면 Windows 방화벽에서 8787 포트 인바운드를 허용해야 합니다.

## 저장 구조

```
<저장 폴더>/rec/<카메라ID>/20261114-013811.mp4   녹화 (시작 시각이 파일 이름)
<저장 폴더>/live/<카메라ID>/index.m3u8           실시간 보기용 임시 파일
C:\ProgramData\mySurveillance\config.json         설정
C:\ProgramData\mySurveillance\logs\               로그
```

녹화 파일은 전원이 갑자기 꺼져도 그때까지 저장된 부분이 재생되는 형식(fragmented MP4)입니다.

## API (모바일 뷰어용)

인증: `POST /api/v1/login` 으로 받은 토큰을 `Authorization: Bearer <token>` 헤더(앱) 또는 쿠키(브라우저)로 보냅니다. 토큰은 90일간 유효합니다.

| 메서드 | 경로 | 설명 |
| --- | --- | --- |
| POST | `/api/v1/login` | `{"password": "..."}` → `{"token": "..."}` |
| POST | `/api/v1/logout` | 현재 토큰 폐기 |
| POST | `/api/v1/password` | `{"current": "...", "new": "..."}` 다른 기기는 모두 로그아웃 |
| GET | `/api/v1/status` | 카메라별 상태, 저장 공간 |
| GET / PUT | `/api/v1/config` | 저장 설정과 카메라 목록 조회 / 변경 |
| GET | `/api/v1/devices` | 이 컴퓨터의 USB 카메라·마이크 목록 |
| GET | `/api/v1/discover` | 카메라 자동 검색 (서버 대역, 장치 목록, 연결 가능 여부, 등록용 초안) |
| POST | `/api/v1/discover/onvif` | `{"xaddr","username","password"}` → ONVIF 카메라의 RTSP 주소 |
| GET | `/api/v1/recordings?camera=ID&from=RFC3339&to=RFC3339` | 녹화 구간 목록 |
| GET | `/rec/<카메라ID>/<파일>` | 녹화 재생 (Range 지원). `?download=1` 이면 다운로드 |
| GET | `/live/<카메라ID>/index.m3u8` | 실시간 HLS 스트림 |

## 빌드

Go 1.24 이상이 필요합니다.

```
go build -o dist/mysurveillance.exe .
```

`ffmpeg` 는 실행 파일 옆, 또는 PATH에서 찾습니다. ffmpeg까지 하나의 exe에 담으려면 `third_party/ffmpeg/ffmpeg.exe` 를 두고 빌드합니다.

```
go build -tags embedffmpeg -o dist/mysurveillance.exe .
```

FFmpeg는 별도 프로그램이며 자체 라이선스(GPL/LGPL)를 따릅니다. FFmpeg를 포함해 배포할 때는 해당 빌드의 라이선스 문서를 함께 제공하세요. hls.js(`web/hls.min.js`)는 Apache-2.0 입니다.
