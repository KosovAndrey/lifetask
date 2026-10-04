# lifetask.ru — HTTPS. Лежит как .tpl: nginx грузит только *.conf, а без
# сертификата не стартует. После выпуска серта копируется в lifetask-ssl.conf.
server {
    listen      443 ssl;
    listen      [::]:443 ssl;
    http2 on;
    server_name lifetask.ru www.lifetask.ru;

    ssl_certificate     /etc/letsencrypt/live/lifetask.ru/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/lifetask.ru/privkey.pem;
    include             /etc/letsencrypt/options-ssl-nginx.conf;
    ssl_dhparam         /etc/letsencrypt/ssl-dhparams.pem;

    # Docker DNS: lp_app резолвится в момент запроса, nginx стартует и без lifeplan.
    resolver 127.0.0.11 valid=10s ipv6=off;
    resolver_timeout 5s;
    set $lifeplan http://lp_app:8090;

    # Личный сервис — из поиска убираем целиком.
    add_header X-Robots-Tag "noindex, nofollow" always;
    add_header Strict-Transport-Security "max-age=31536000" always;
    add_header Permissions-Policy "camera=(), microphone=(), geolocation=()" always;
    add_header Cross-Origin-Opener-Policy "same-origin" always;
    # CSP и nosniff выставляет само приложение.

    client_max_body_size 60m;   # вложения до 50 МБ

    location / {
        proxy_pass         $lifeplan;
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_set_header   X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_read_timeout 180s;   # загрузка файла в Drive бывает долгой
    }
}
