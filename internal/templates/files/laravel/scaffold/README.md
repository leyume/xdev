# Laravel app

Place your Laravel application's files in this directory (this is bind-mounted
to `/var/www/html` inside the Swoole container).

Quick start with a fresh app:

```bash
# from this directory (the app/ folder of your xdev Laravel app)
composer create-project laravel/laravel .
composer require laravel/octane
php artisan octane:install --server=swoole
```

Then **Start** (or restart) the app in xdev. The container runs:

```
php artisan octane:start --server=swoole --host=0.0.0.0 --port=8000
```

Database & cache are already wired (service names `db` and `redis`):

```
DB_HOST=db   DB_DATABASE=laravel   DB_USERNAME=laravel   DB_PASSWORD=secret
REDIS_HOST=redis
```
