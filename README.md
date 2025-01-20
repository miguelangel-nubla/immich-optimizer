# Immich Upload Optimizer

Immich Upload Optimizer is a proxy designed to be placed in front of the Immich server. It intercepts image uploads and uses an external CLI program (by default, [Caesium](https://github.com/Lymphatus/caesium-clt)) to optimize, resize, or compress images before they are stored on the Immich server. This helps save storage space on the Immich server by reducing the size of uploaded images.

## Features

- Intercepts image uploads to the Immich server.
- You can use any external CLI program to optimize, resize, or compress images.
- Designed to be easily integrated into existing Immich installations using Docker Compose.

## Usage via docker compose

1. Update your Docker Compose configuration to route incoming connections through the proxy:

    ```yaml
    services:
      immich-upload-optimizer:
        image: ghcr.io/miguelangel-nubla/immich-upload-optimizer-caesium:latest
        ports:
          - "2283:2283"
        environment:
          - IUO_UPSTREAM=http://immich-server:2283
          - IUO_LISTEN=:2283
          - IUO_CONVERT_CMD=caesiumclt --keep-dates --exif --quality=0 --output={{.dirname}} {{.filename}}
          - IUO_FILTER_PATH=/api/assets
          - IUO_FILTER_FORM_KEY=assetData
        depends_on:
          - immich-server

      immich-server:
        # ...existing configuration...
        # remove the ports section so incoming requests are handled by the proxy by default
    ```

2. Restart your Docker Compose services:

    ```sh
    docker-compose restart
    ```

## Available flags

  - `-upstream`: The URL of the Immich server (e.g., `http://immich-server:2283`).
  - `-listen`: The address on which the proxy will listen (default: `:2283`).
  - `-convert_cmd`: Command to apply to convert image, available placeholders: `{{.dirname}}`, `{{.filename}}`.
    - Default: `caesiumclt --keep-dates --exif --quality=0 --output={{.dirname}} {{.filename}}`. (0 equals lossless compression)
    - This utility will read the converted file from the same filename, so you need to overwrite the original.
    - The file is in a temp folder by itself.
  - `-filter-path`: The path to filter image uploads (default: `/api/assets`).
  - `-filter-form-key`: The form key to filter image uploads (default: `assetData`).

  All flags are available as enviroment variables using the prefix `IUO_`.

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.

## Contributing

Contributions are welcome! Please open an issue or submit a pull request on GitHub.

## About This Project 

This project is a complete rewrite from scratch of the original idea by [JamesCullum/multipart-upload-proxy](https://github.com/JamesCullum/multipart-upload-proxy). It has been designed with the following key goals:

- **Transparent Proxy for Immich**  
  Eliminates the need for Cloudflare or reverse proxies with path redirection, offering seamless integration.

- **Extensibility**  
  Designed to support any CLI program or custom script, enabling custom workflows for image processing.

## Acknowledgements

- [JamesCullum/multipart-upload-proxy](https://github.com/JamesCullum/multipart-upload-proxy) for the original idea.
- [Caesium](https://github.com/Lymphatus/caesium) for the image compression tool.
- [Immich](https://github.com/immich-app/immich) for the self-hosted photo and video backup solution.